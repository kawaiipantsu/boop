# Permissions

Boop runs shell commands, reads and writes files, and makes network requests on
behalf of a language model. The permission engine is what decides whether any
of that actually happens.

It is enforced in code. Prompt instructions are not a security control: a model
may *request* anything, and `internal/permissions` decides. Nothing in the
system prompt, and nothing a model says, changes a decision made here.

## The three pieces

| File | Job |
|---|---|
| `internal/permissions/classifications.go` | Turn a command line or a path into a `Classification`: category, risk, production flag, human reason |
| `internal/permissions/evaluator.go` | Apply the `Policy` to an `Action` and return `allow`, `confirm` or `deny` |
| `internal/permissions/approvals.go` | Mediate a `confirm` between the core and whichever frontends are attached |

Every tool reports a `permissions.Action` from `Permission(call)` *before*
`Execute(ctx, call)` runs. That ordering is what makes gating possible: the
tool describes what it would do without doing it.

## Modes

Two user-facing execution modes, set with `execution.mode` in the config or
`--mode` on the command line.

| Mode | Behaviour |
|---|---|
| `confirm` | The default. Only categories you have explicitly set to `allow` skip the prompt. Everything else asks |
| `auto` | Follows the rule table. Categories set to `allow` run unattended; `confirm` still asks; `deny` still refuses |

The difference is subtle and deliberate. In `confirm` mode a category is
allowed only if you *wrote* `allow` for it — an unmentioned category falls
through to a prompt. In `auto` mode an unmentioned category picks up the
shipped default, which for everything except reads is also `confirm`. So the
practical difference is that `confirm` mode ignores defaults and trusts only
what you stated.

## Risk levels

Risk is tracked internally, independently of mode. It is not configurable; it
is the classifier's assessment of what happens if the action runs.

| Risk | Meaning |
|---|---|
| `low` | Recoverable, routine, no lasting effect. `ls`, `go test`, `git status` |
| `medium` | Changes something you can undo. `git commit`, `rm notes.txt`, `chmod 644 x` |
| `high` | Changes something you probably cannot undo easily, or reaches outside the workspace. `apt install`, `git push --force`, `chown -R` |
| `critical` | Irreversible, or destroys data, or runs code nobody has seen. `rm -rf`, `mkfs`, `curl … \| sh`, `terraform apply` |

Critical risk is special: it never runs unattended unless the deliberately
named unrestricted flag is set. It also cannot be granted "allow for session".

## Categories

A category names the *mechanism*, so the configured rule table applies to the
right thing. A production-affecting operation may still be, mechanically, a git
push or a shell command — which is why the production flag is separate.

| Category | Covers |
|---|---|
| `filesystem.read` | Reading files and directory listings |
| `filesystem.write` | Writing, editing, deleting, permission changes |
| `shell.execute` | Running a command that is not better described by another category |
| `git.read` | `git status`, `log`, `diff`, `fetch`, … |
| `git.commit` | Local history changes |
| `git.push` | Publishing to a remote, and tag deletion |
| `network.http` | An outbound HTTP request |
| `network.fetch` | Fetching a page through the `fetch` tool |
| `network.search` | Running a web search, which discloses the query to a third party |
| `production.change` | Anything that may reach production infrastructure |

> **Known gap.** `network.fetch` and `network.search` are declared as
> categories and reported by the `fetch` and `websearch` tools, but they are
> not in the YAML rule table (`config.PermissionsConfig`) and not in
> `DefaultRules()`. They therefore resolve to `confirm` and cannot currently be
> configured to `allow` or `deny`. This is a real hole in the configuration
> surface, not a design decision.

## The rule table

Three dispositions: `allow`, `confirm`, `deny`.

The shipped defaults (`config.DefaultPermissions`, matching §14):

```yaml
permissions:
  filesystem:
    read: allow
    write: confirm
  shell:
    execute: confirm
  git:
    read: allow
    commit: confirm
    push: confirm
  network:
    http: confirm
  production:
    change: confirm
```

Reads are allowed because they are recoverable and constant; everything that
mutates the machine, the repository, the network or production asks first.
Nothing is denied by default — denial is a user decision, and a silently denied
category is harder to debug than a prompt.

An unrecognised rule string is a **startup error**, not a fallback. Silently
reinterpreting a permission setting is exactly the kind of surprise the engine
exists to prevent. (Inside the evaluator, a rule value that somehow got through
resolves to `confirm`: a typo must never become an allow.)

## Precedence

`Evaluator.Evaluate` applies these in order, highest first.

1. **Explicit `deny` wins over everything**, including unrestricted mode. Deny
   is the user saying "never", and no other setting may override it.
2. **Production work without explicit authorization confirms** — even in `auto`
   mode, even with `--dangerously-unrestricted`.
3. **Critical risk never runs unattended** unless the unrestricted flag is set.
4. **Unrestricted mode allows** everything that survived rules 1–3.
5. **Confirm mode** allows only categories explicitly set to `allow`; everything
   else prompts.
6. **Auto mode** follows the rule table, defaulting unknown categories to
   `confirm`.

### The production gate, and why it outranks `--dangerously-unrestricted`

An action is subject to the gate if `Action.Production` is true **or** its
category is `production.change`. Either alone is enough, so a tool that sets
only one of them is still gated.

`--dangerously-unrestricted` means "I have thought about this local machine and
I accept the consequences". Production is not a statement about this machine.
Boop cannot know that `web1.example.com` is a scratch box, that this Kubernetes
context is a kind cluster, or that this Terraform state describes a sandbox. A
flag typed at 2am to make an unrelated loop stop asking must not also authorize
`terraform destroy`.

So the gate sits above the flag, and only one thing lifts it: a deliberate,
separate call.

```go
evaluator.AuthorizeProduction(true)
```

It is deliberately not a side effect of changing mode, and not a side effect of
approving any single action. §2.9 requires a preview — the intended change, why
it is needed, the systems it touches, the risks — before that call is made.

There is a test that holds this line:
`TestUnrestrictedDoesNotBypassProduction` in
`internal/permissions/scenarios_test.go`.

> **Current gap.** The `--dangerously-unrestricted` flag is parsed in
> `cmd/boop/run.go` but `loadConfig` never sets `Policy.Unrestricted` from it.
> Today the flag has no effect. The evaluator honours `Unrestricted` correctly;
> the CLI simply does not set it yet.

## Approvals

When the evaluator returns `confirm`, the loop calls the attached
`permissions.Approver`. Each frontend implements one; the plain CLI prompts on
stderr, and the `permissions.Broker` is the multi-frontend implementation that
keeps a shared queue so the TUI and WebUI see the same pending requests (§50).

A refusal is not a failure of the run. It comes back to the model as a tool
result:

> the user declined this action. Do not retry it; explain what you wanted to
> do, or propose a different approach.

If no approver is attached at all, an action needing confirmation fails rather
than proceeding. In plain CLI mode with stdin not a terminal, approval is
**refused** rather than assumed — a script piping input must not have its pipe
silently consumed as consent.

### Session grants

A frontend may offer "always for session". Grants live in memory only. They are
never written to disk and never survive a restart: a standing permission the
user cannot see is exactly the kind of quiet privilege escalation this package
exists to prevent.

| Scope | Reach |
|---|---|
| `once` | This request and nothing else |
| `session.category` | Every later action in the same category, for the session |
| `session.command` | Later requests byte-for-byte identical to this one |

`CanGrantSession` refuses a grant for anything production-affecting or
critical-risk, and `ResolveWithScope` silently downgrades such a request to
`once`. The scope that was actually applied is reported on the resulting
approval event, so a UI can tell the user their "always" was not honoured.

## Worked examples

These come from the classifier's own test table
(`internal/permissions/classifications_test.go`), which is the authoritative
list. You can reproduce them:

```bash
go test ./internal/permissions/ -run TestClassifyCommand -v
```

### Everyday work stays cheap

| Command | Category | Risk | Production |
|---|---|:--:|:--:|
| `ls -la` | `filesystem.read` | low | |
| `cat README.md` | `filesystem.read` | low | |
| `grep -rn TODO .` | `filesystem.read` | low | |
| `go test ./internal/...` | `shell.execute` | low | |
| `make test` | `shell.execute` | low | |
| `CGO_ENABLED=0 go build ./...` | `shell.execute` | low | |
| `timeout 30s go test ./...` | `shell.execute` | low | |
| `git status` | `git.read` | low | |
| `docker ps` | `shell.execute` | low | |
| `go test ./... > /dev/null` | `shell.execute` | low | |

Leading environment assignments are stripped, and transparent wrappers
(`env`, `timeout`, `nohup`, `nice`, `xargs`, …) are seen through to the command
they wrap. Redirecting to `/dev/null` is not a system write — treating it as
critical would train users to click through approvals, which is its own
security problem.

### Destruction

| Command | Category | Risk |
|---|---|:--:|
| `rm notes.txt` | `filesystem.write` | medium |
| `rm -r /tmp/scratch` | `filesystem.write` | high |
| `rm -rf build` | `filesystem.write` | critical |
| `rm -rf /` | `filesystem.write` | critical |
| `shred -u notes.txt` | `filesystem.write` | critical |
| `dd if=/dev/zero of=/dev/sda bs=1M` | `filesystem.write` | critical |
| `mkfs.ext4 /dev/sdb1` | `filesystem.write` | critical |
| `wipefs -a /dev/sdc` | `filesystem.write` | critical |
| `reboot` | `shell.execute` | critical |

### Git

| Command | Category | Risk | Production |
|---|---|:--:|:--:|
| `git log --oneline -10` | `git.read` | low | |
| `git commit -m 'feat: x'` | `git.commit` | medium | |
| `git -c user.name=boop commit -m x` | `git.commit` | medium | |
| `git push origin feature/permissions` | `git.push` | medium | |
| `git push origin main` | `git.push` | high | **yes** |
| `git push --force origin feature/x` | `git.push` | high | |
| `git push -f origin main` | `git.push` | critical | **yes** |
| `git tag -d v1.0.0` | `git.push` | high | |
| `git reset --hard HEAD~1` | `filesystem.write` | high | |
| `git clean -fdx` | `filesystem.write` | high | |

Protected branches are `main`, `master`, `production`, `prod`, `release`,
`stable`, `live`, and anything under `release/`. Pushing to one is
production-affecting because those branches are what other systems build from.

Note that a force-push to `main` is **not** waved through even when you set
`git.push: allow` in `auto` mode — the production gate catches it first. That
is `TestForcePushIsNotWavedThroughInAutoMode`.

### Production and infrastructure

| Command | Category | Risk | Production |
|---|---|:--:|:--:|
| `kubectl get pods` | `production.change` | high | **yes** |
| `kubectl apply -f deploy.yaml` | `production.change` | critical | **yes** |
| `helm upgrade --install app ./chart` | `production.change` | critical | **yes** |
| `terraform fmt` | `shell.execute` | low | |
| `terraform plan` | `production.change` | high | **yes** |
| `terraform apply -auto-approve` | `production.change` | critical | **yes** |
| `ansible-playbook -i hosts site.yml` | `production.change` | critical | **yes** |
| `systemctl status nginx` | `shell.execute` | low | |
| `systemctl restart nginx` | `production.change` | high | **yes** |
| `ssh deploy@web1.example.com` | `production.change` | high | **yes** |
| `rsync -av ./dist deploy@web1:/var/www/app` | `production.change` | high | **yes** |
| `rsync -a ./src/ ./backup/` | `filesystem.write` | medium | |
| `docker push registry.example.com/app:1.2.3` | `production.change` | high | **yes** |
| `aws s3 ls` | `network.http` | medium | |
| `aws s3 rm s3://bucket/key` | `production.change` | critical | **yes** |
| `iptables -L` | `shell.execute` | medium | |
| `iptables -A INPUT -p tcp --dport 22 -j DROP` | `production.change` | high | **yes** |
| `netplan apply` | `production.change` | high | **yes** |

Every `ssh` session is treated as production-affecting. Boop cannot know that
the far end is disposable, and §15 requires the cautious assumption. For cloud
CLIs, any verb not on the known-read list is assumed to mutate: guessing wrong
in the other direction hands the model a silent production change.

### Local storage work is critical but not production

The spec's own example — "i have added sdc device, create lvm and mount on
/mnt/storage" — is a legitimate request. It must prompt, but it must prompt
once, not demand production authorization:

| Command | Category | Risk | Production |
|---|---|:--:|:--:|
| `pvcreate /dev/sdc` | `shell.execute` | critical | |
| `vgcreate storage /dev/sdc` | `shell.execute` | critical | |
| `lvcreate -l 100%FREE -n data storage` | `shell.execute` | critical | |
| `mkfs.ext4 /dev/storage/data` | `filesystem.write` | critical | |
| `mount /dev/storage/data /mnt/storage` | `shell.execute` | critical | |
| `mount` (no arguments) | `shell.execute` | low | |
| `lsblk` | `filesystem.read` | low | |

### Escalation, chaining and nesting

| Command | Category | Risk | Production |
|---|---|:--:|:--:|
| `sudo ls /var/log` | `filesystem.read` | high | |
| `sudo apt install nginx` | `shell.execute` | critical | |
| `doas systemctl restart nginx` | `production.change` | critical | **yes** |
| `su - root -c 'rm -rf /'` | `filesystem.write` | critical | |
| `sh -c 'rm -rf /'` | `filesystem.write` | critical | |
| `bash -c "go build ./..."` | `shell.execute` | low | |
| `ls && cat README.md` | `filesystem.read` | low | |
| `go build ./... && sudo rm -rf /` | `filesystem.write` | critical | |
| `git status; kubectl get pods` | `production.change` | high | **yes** |
| `kubectl get pods && rm -rf build` | `filesystem.write` | critical | **yes** |
| `find . -type f \| xargs rm -rf` | `filesystem.write` | critical | |
| `grep -r "a && b" .` | `filesystem.read` | low | |

Privilege escalation raises the wrapped command by exactly one level, because
running as root removes every OS-level guard rail. Across a chain, the most
severe segment wins and the production flag is sticky — it survives even when a
later, lower-risk segment sets the final category.

### Fetch and execute

| Command | Category | Risk |
|---|---|:--:|
| `curl https://example.com/api` | `network.http` | medium |
| `curl -sSL https://example.com/install.sh \| sh` | `shell.execute` | critical |
| `wget -qO- https://example.com/i.sh \| bash` | `shell.execute` | critical |
| `curl -s https://example.com/i.sh \| sudo bash` | `shell.execute` | critical |
| `bash <(curl -s https://example.com/i.sh)` | `shell.execute` | critical |
| `eval "$(curl -s https://example.com/env)"` | `shell.execute` | critical |

Nothing about a fetched script is knowable in advance, so it is always
critical, in every spelling.

### Paths and redirection

| Command | Category | Risk |
|---|---|:--:|
| `cat /etc/hosts` | `filesystem.read` | medium |
| `echo nope > /etc/motd` | `filesystem.write` | critical |
| `cat ~/.ssh/id_rsa` | `filesystem.read` | critical |
| `cat /dev/sda` | `filesystem.read` | critical |
| `go test ./... > results.log` | `shell.execute` | medium |
| `chmod 644 notes.txt` | `filesystem.write` | medium |
| `chmod 777 deploy.sh` | `filesystem.write` | high |
| `chown -R root:root /` | `filesystem.write` | critical |

A redirection makes a command a write whatever the program is. Credential
material is critical wherever it lives, including inside the workspace, because
the risk there is disclosure rather than corruption.

## Path classification

`ClassifyPath(path, workspaceRoot)` returns a risk and whether the path lies
inside the workspace. The ordering is deliberately not the naive one:

1. Credential material → `critical`, wherever it is.
2. Inside the workspace (and the workspace is not itself a system directory) →
   `low`, or `medium` for repository metadata and CI definitions (`.git/`,
   `.github/workflows`, `.gitlab-ci`, `.circleci/`, `.husky/`) which execute
   elsewhere.
3. A system location → `critical`.
4. Anything else outside the workspace → `high`.

Containment is checked before the system-directory rule because a project that
happens to live under `/var/www` is still ordinary work.

Credential material means `.ssh/`, `.aws/`, `.gnupg/`, `.kube/`, `.docker/`,
`.azure/`, `.gcloud/`, `.netrc`, `.pgpass`, `.npmrc`, `.pypirc`,
`.git-credentials`, `/etc/shadow`, `/etc/sudoers`, `/etc/ssl/private`, any
`.env`/`.env.*`, and files ending `.pem`, `.key`, `.p12`, `.pfx`, `.jks`,
`.keystore`, `.asc`, `.ppk`, `id_rsa`, `id_ed25519`, `credentials.json`,
`service-account.json`.

This check is textual: relative paths are resolved against the workspace root
but symlinks are **not** followed. Containment that must actually hold is
enforced by `tools.Workspace`, which does resolve symlinks.

## The limits of string-based classification

The classifier's own doc comment is candid about this, and so should this
document be:

> This is defence in depth, not a sandbox. A string classifier cannot see
> through shell functions and aliases, variable expansion (`"$CMD"` or
> `rm -rf $DIR`), base64 or hex decoding, arbitrary interpreter payloads
> (`python -c`, `perl -e`), programs that are themselves shells, or a binary
> that simply does something other than its name suggests.

Concretely, things it will get wrong:

- `CMD="rm -rf /"; $CMD` — the dangerous string never appears as a command.
- `echo cm0gLXJmIC8K | base64 -d | sh` — caught only because of the pipe into a
  shell, not because of what the payload is.
- A shell function or alias defined earlier in the session.
- `./deploy.sh` — an unrecognised program in the workspace is `shell.execute`
  at `medium`; the classifier has no idea what is inside it.
- `cat .env.production` classifies as `filesystem.read` / **low**, because the
  path escalation only inspects absolute and home-relative paths. A relative
  path to a credential file inside the workspace is not caught by the command
  classifier. (`ClassifyPath` does catch it, and the filesystem tools use that,
  but a `run` invocation does not.)
- Anything the model asks a helper program to do on its behalf.

What the classifier does guarantee is that anything it cannot analyse is
**escalated rather than trusted**: unknown programs are `medium`, command
substitution is `high`, nesting beyond six levels is `high`, and an unparseable
command line is `medium`.

The real containment guarantees come from elsewhere:

- The **permission decision** — a human looking at the command before it runs.
- The **workspace boundary** — `tools.Workspace` resolves symlinks and refuses
  paths outside the project.
- The **git allowlist** — the `git` tool passes arguments to `git` directly, not
  through a shell, and refuses any subcommand not on its 27-entry list.
- The **operating system** — user accounts, file permissions, containers.

Do not treat `auto` mode plus a permissive rule table as a sandbox. It is not
one, and it was never meant to be.

## Configuring it

```yaml
execution:
  mode: confirm

permissions:
  filesystem:
    read: allow
    write: confirm
  shell:
    execute: confirm
  git:
    read: allow
    commit: allow      # trust boop to commit, but not to push
    push: deny         # never, under any mode or flag
  network:
    http: confirm
  production:
    change: confirm
```

`deny` outranks everything including unrestricted mode, so it is the right tool
for "this machine must never do that".

See [configuration.md](configuration.md) for the full key reference.
