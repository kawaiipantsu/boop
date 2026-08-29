package permissions

import (
	"os"
	"regexp"
	"strings"
)

// Classification is the static verdict on a command line or filesystem path.
//
// It is input to the Evaluator, not a decision: the classifier decides *what
// kind of operation* something is, the Evaluator decides whether it may run.
type Classification struct {
	// Category is the permission category the operation falls under. It
	// reflects the mechanism (git, shell, filesystem, network) so the
	// configured rule table applies to the right thing.
	Category Category `json:"category"`
	// Risk is the severity of the operation if it runs.
	Risk Risk `json:"risk"`
	// Production reports that the operation may reach production
	// infrastructure and therefore needs explicit authorization (§15).
	// It is tracked separately from Category because a production-affecting
	// operation may still be, mechanically, a git push or a shell command.
	Production bool `json:"production"`
	// Reason is a short human explanation of the verdict, written so it can
	// be shown verbatim in an approval prompt.
	Reason string `json:"reason"`
	// Matched records the token or pattern that drove the verdict. It exists
	// for logs and debugging, not for display.
	Matched string `json:"matched,omitempty"`
}

// Action converts a Classification into an Action ready for evaluation.
//
// tool is the registered tool making the request, summary is the one-line
// description shown to the user, and detail carries the specifics under
// review (typically the command line itself).
func (c Classification) Action(tool, summary, detail string) Action {
	return Action{
		Category:   c.Category,
		Risk:       c.Risk,
		Tool:       tool,
		Summary:    summary,
		Detail:     detail,
		Production: c.Production,
	}
}

// riskSeverity orders risk levels. An unset or unrecognised Risk deliberately
// sorts below RiskLow so that MaxRisk can start from the zero value; the
// Evaluator never treats an unrecognised risk as critical, so classifiers must
// always set one explicitly.
var riskSeverity = map[Risk]int{
	RiskLow:      0,
	RiskMedium:   1,
	RiskHigh:     2,
	RiskCritical: 3,
}

// Severity returns the ordinal rank of r, or -1 if r is unset or unknown.
func (r Risk) Severity() int {
	if s, ok := riskSeverity[r]; ok {
		return s
	}
	return -1
}

// AtLeast reports whether r is at least as severe as other.
func (r Risk) AtLeast(other Risk) bool { return r.Severity() >= other.Severity() }

// MaxRisk returns the most severe of the given risks.
func MaxRisk(risks ...Risk) Risk {
	worst := Risk("")
	for _, r := range risks {
		if r.Severity() > worst.Severity() {
			worst = r
		}
	}
	return worst
}

// EscalateRisk returns the next risk level up, saturating at RiskCritical.
// It is used where a wrapper (such as sudo) makes an operation strictly more
// dangerous than the command it wraps.
func EscalateRisk(r Risk) Risk {
	switch r {
	case RiskLow:
		return RiskMedium
	case RiskMedium:
		return RiskHigh
	case RiskHigh, RiskCritical:
		return RiskCritical
	default:
		// Unset or unknown: assume the worst we can justify.
		return RiskHigh
	}
}

// maxClassifyDepth bounds recursion through wrappers such as sudo, `sh -c`
// and xargs so a crafted command line cannot make classification loop.
const maxClassifyDepth = 6

// ClassifyCommand statically classifies a shell command line.
//
// It understands chaining (&&, ||, ;, |), leading environment assignments,
// common quoting, privilege-escalation wrappers, and `sh -c` nesting; every
// segment is classified and the most severe verdict wins, with Production
// true if any segment touches production.
//
// This is defence in depth, not a sandbox. A string classifier cannot see
// through shell functions and aliases, variable expansion ("$CMD" or
// "rm -rf $DIR"), base64 or hex decoding, arbitrary interpreter payloads
// (python -c, perl -e), programs that are themselves shells, or a binary that
// simply does something other than its name suggests. Anything it cannot
// analyse is escalated rather than trusted, and the real containment
// guarantees must come from the permission decision, the workspace boundary
// and the operating system - never from this function alone.
func ClassifyCommand(cmdline string) Classification {
	return classifyCommandLine(cmdline, 0)
}

func classifyCommandLine(cmdline string, depth int) Classification {
	if depth > maxClassifyDepth {
		return Classification{
			Category: CatShellExecute,
			Risk:     RiskHigh,
			Reason:   "command nests too deeply to analyse reliably",
		}
	}
	trimmed := strings.TrimSpace(cmdline)
	if trimmed == "" {
		return Classification{Category: CatShellExecute, Risk: RiskLow, Reason: "empty command"}
	}

	worst := Classification{
		Category: CatShellExecute,
		Risk:     RiskMedium,
		Reason:   "command could not be parsed; treated as medium risk",
	}
	found := false
	production := false

	consider := func(c Classification) {
		production = production || c.Production
		if !found || c.Risk.Severity() > worst.Risk.Severity() {
			worst, found = c, true
		}
	}

	for _, pipeline := range splitPipelines(trimmed) {
		argvs := make([][]string, 0, len(pipeline))
		for _, segment := range pipeline {
			argv := stripEnvAssignments(tokenize(segment))
			if len(argv) == 0 {
				continue
			}
			argvs = append(argvs, argv)
			consider(escalateForArguments(classifyArgv(argv, depth), argv, segment))
		}
		if c, ok := classifyFetchExec(argvs); ok {
			consider(c)
		}
	}
	if c, ok := classifyProcessSubstitution(trimmed); ok {
		consider(c)
	}

	worst.Production = worst.Production || production
	return worst
}

// ---------------------------------------------------------------------------
// Lexing
// ---------------------------------------------------------------------------

// splitPipelines splits a command line into pipelines, and each pipeline into
// its simple commands. Quoting is honoured so a separator inside a quoted
// string is not treated as one.
func splitPipelines(cmdline string) [][]string {
	var (
		pipelines [][]string
		current   []string
		sb        strings.Builder
	)
	flushSegment := func() {
		if s := strings.TrimSpace(sb.String()); s != "" {
			current = append(current, s)
		}
		sb.Reset()
	}
	flushPipeline := func() {
		flushSegment()
		if len(current) > 0 {
			pipelines = append(pipelines, current)
			current = nil
		}
	}

	runes := []rune(cmdline)
	var quote rune
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		switch {
		case quote != 0:
			sb.WriteRune(ch)
			if ch == '\\' && quote == '"' && i+1 < len(runes) {
				i++
				sb.WriteRune(runes[i])
				continue
			}
			if ch == quote {
				quote = 0
			}
		case ch == '\'' || ch == '"':
			quote = ch
			sb.WriteRune(ch)
		case ch == '\\' && i+1 < len(runes):
			sb.WriteRune(ch)
			i++
			sb.WriteRune(runes[i])
		case ch == '\n' || ch == ';':
			flushPipeline()
		case ch == '&':
			if i+1 < len(runes) && runes[i+1] == '&' {
				i++
				flushPipeline()
				continue
			}
			// A bare "&" only backgrounds when it ends a command; inside
			// "2>&1" it is part of a redirection.
			if i+1 >= len(runes) || runes[i+1] == ' ' || runes[i+1] == '\t' || runes[i+1] == '\n' {
				flushPipeline()
				continue
			}
			sb.WriteRune(ch)
		case ch == '|':
			if i+1 < len(runes) && runes[i+1] == '|' {
				i++
				flushPipeline()
				continue
			}
			flushSegment()
		default:
			sb.WriteRune(ch)
		}
	}
	flushPipeline()
	return pipelines
}

// tokenize splits one simple command into tokens, stripping quotes and
// escapes and emitting redirection operators as separate tokens so their
// targets can be inspected.
func tokenize(segment string) []string {
	var (
		tokens  []string
		sb      strings.Builder
		hasWord bool
	)
	flush := func() {
		if hasWord {
			tokens = append(tokens, sb.String())
		}
		sb.Reset()
		hasWord = false
	}

	runes := []rune(segment)
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		switch ch {
		case ' ', '\t', '\n', '\r':
			flush()
		case '\'':
			hasWord = true
			for i++; i < len(runes) && runes[i] != '\''; i++ {
				sb.WriteRune(runes[i])
			}
		case '"':
			hasWord = true
			for i++; i < len(runes) && runes[i] != '"'; i++ {
				if runes[i] == '\\' && i+1 < len(runes) {
					i++
				}
				sb.WriteRune(runes[i])
			}
		case '\\':
			if i+1 < len(runes) {
				i++
				hasWord = true
				sb.WriteRune(runes[i])
			}
		case '>', '<':
			flush()
			op := string(ch)
			if i+1 < len(runes) && runes[i+1] == ch {
				i++
				op += string(ch)
			}
			tokens = append(tokens, op)
		default:
			hasWord = true
			sb.WriteRune(ch)
		}
	}
	flush()
	return tokens
}

var envAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// stripEnvAssignments removes leading "FOO=bar" assignments and a leading
// `env` invocation so the real command is classified.
func stripEnvAssignments(argv []string) []string {
	i := 0
	for i < len(argv) {
		switch {
		case envAssignRe.MatchString(argv[i]):
			i++
		case commandName(argv[i]) == "env" && i == 0:
			i++
			for i < len(argv) {
				if envAssignRe.MatchString(argv[i]) {
					i++
					continue
				}
				if argv[i] == "-u" || argv[i] == "--unset" || argv[i] == "-C" || argv[i] == "--chdir" {
					i += 2
					continue
				}
				if strings.HasPrefix(argv[i], "-") {
					i++
					continue
				}
				break
			}
		default:
			return argv[i:]
		}
	}
	return nil
}

// commandName reduces an argv[0] to a comparable program name: directory
// stripped, lower-cased, and without a Windows executable suffix.
func commandName(token string) string {
	token = strings.ReplaceAll(token, "\\", "/")
	if i := strings.LastIndex(token, "/"); i >= 0 {
		token = token[i+1:]
	}
	token = strings.ToLower(token)
	for _, ext := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		token = strings.TrimSuffix(token, ext)
	}
	return token
}

// ---------------------------------------------------------------------------
// Small argument helpers
// ---------------------------------------------------------------------------

// hasShortFlag reports whether any argument carries the given short flag,
// including bundled forms such as "-rf".
func hasShortFlag(args []string, flag rune) bool {
	for _, a := range args {
		if len(a) < 2 || a[0] != '-' || strings.HasPrefix(a, "--") {
			continue
		}
		if strings.ContainsRune(a[1:], flag) {
			return true
		}
	}
	return false
}

// hasFlag reports whether any argument exactly equals one of the given flags.
func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f {
				return true
			}
		}
	}
	return false
}

// hasAnyPrefix reports whether any argument starts with one of the prefixes.
func hasAnyPrefix(args []string, prefixes ...string) bool {
	for _, a := range args {
		for _, p := range prefixes {
			if strings.HasPrefix(a, p) {
				return true
			}
		}
	}
	return false
}

// positionals returns the arguments that are not flags or redirections.
func positionals(args []string) []string {
	out := make([]string, 0, len(args))
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if a == ">" || a == ">>" || a == "<" || a == "<<" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// firstPositional returns the first non-flag argument, or "".
func firstPositional(args []string) string {
	if p := positionals(args); len(p) > 0 {
		return p[0]
	}
	return ""
}

// containsWord reports whether set contains s.
func containsWord(set map[string]struct{}, s string) bool {
	_, ok := set[s]
	return ok
}

// words builds a set from a list, for readable membership tests.
func words(list ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(list))
	for _, w := range list {
		m[w] = struct{}{}
	}
	return m
}

func cls(cat Category, risk Risk, reason, matched string) Classification {
	return Classification{Category: cat, Risk: risk, Reason: reason, Matched: matched}
}

func prodCls(cat Category, risk Risk, reason, matched string) Classification {
	c := cls(cat, risk, reason, matched)
	c.Production = true
	return c
}

// raise lifts c to at least risk, replacing the reason when it does. It never
// lowers a verdict.
func raise(c Classification, risk Risk, reason, matched string) Classification {
	if risk.Severity() > c.Risk.Severity() {
		c.Risk = risk
		c.Reason = reason
		if matched != "" {
			c.Matched = matched
		}
	}
	return c
}

// ---------------------------------------------------------------------------
// Command families
// ---------------------------------------------------------------------------

var (
	privilegeEscalators = words("sudo", "doas", "pkexec", "su", "runas", "gosu")

	// shellInterpreters run whatever string they are handed.
	shellInterpreters = words("sh", "bash", "zsh", "dash", "ksh", "csh", "tcsh",
		"fish", "ash", "busybox", "pwsh", "powershell", "cmd", "iex",
		"invoke-expression")

	// scriptInterpreters execute code but are not shells.
	scriptInterpreters = words("python", "python2", "python3", "perl", "ruby",
		"node", "deno", "bun", "php", "lua", "tclsh", "osascript")

	// fetchers download remote content.
	fetchers = words("curl", "wget", "iwr", "invoke-webrequest", "aria2c",
		"httpie", "http", "fetch")

	// transparent wrappers run another command; classify what they wrap.
	commandWrappers = words("time", "nohup", "command", "builtin", "exec",
		"setsid", "stdbuf", "unbuffer", "script", "xargs", "watch", "nice",
		"ionice", "chrt", "taskset", "timeout", "strace", "ltrace")

	// readOnlyCommands inspect the machine without changing it.
	readOnlyCommands = words("ls", "dir", "cat", "bat", "head", "tail", "less",
		"more", "grep", "egrep", "fgrep", "rg", "ag", "ack", "find", "fd",
		"locate", "wc", "stat", "file", "du", "df", "tree", "which", "type",
		"whereis", "whoami", "id", "groups", "uname", "hostname", "date",
		"uptime", "pwd", "echo", "printf", "sort", "uniq", "cut", "tr", "jq",
		"yq", "column", "diff", "cmp", "md5sum", "sha256sum", "basename",
		"dirname", "readlink", "realpath", "lsblk", "blkid", "lscpu", "lsusb",
		"lspci", "free", "ps", "env", "printenv", "history", "man", "help")

	// buildCommands compile or test code inside the workspace.
	buildCommands = words("make", "cmake", "ninja", "gradle", "mvn", "dotnet",
		"go", "cargo", "npm", "yarn", "pnpm", "bun", "pytest", "tox", "jest",
		"vitest", "phpunit", "rake", "gcc", "g++", "clang", "javac", "tsc",
		"eslint", "prettier", "gofmt", "golangci-lint", "ruff", "black",
		"mypy", "shellcheck")
)

// classifyArgv classifies one simple command, already split into tokens with
// environment assignments removed.
func classifyArgv(argv []string, depth int) Classification {
	if len(argv) == 0 {
		return cls(CatShellExecute, RiskLow, "empty command", "")
	}
	if depth > maxClassifyDepth {
		return cls(CatShellExecute, RiskHigh, "command nests too deeply to analyse reliably", "")
	}
	name := commandName(argv[0])
	args := argv[1:]

	switch {
	case containsWord(privilegeEscalators, name):
		return classifyPrivileged(name, args, depth)
	case containsWord(shellInterpreters, name):
		return classifyShellInterpreter(name, args, depth)
	case containsWord(commandWrappers, name):
		return classifyWrapper(name, args, depth)
	}

	if c, ok := classifyDestructive(name, args); ok {
		return c
	}
	if c, ok := classifyStorage(name, args); ok {
		return c
	}
	if name == "git" {
		return classifyGit(args)
	}
	if c, ok := classifyDeployment(name, args, depth); ok {
		return c
	}
	if c, ok := classifyCloud(name, args); ok {
		return c
	}
	if c, ok := classifyNetworkConfig(name, args); ok {
		return c
	}
	if c, ok := classifyAccounts(name, args); ok {
		return c
	}
	if c, ok := classifyPackages(name, args); ok {
		return c
	}
	if c, ok := classifyOrdinary(name, args, depth); ok {
		return c
	}

	// Unknown programs are not assumed safe.
	return cls(CatShellExecute, RiskMedium,
		"runs "+name+", which Boop does not recognise", name)
}

// classifyPrivileged classifies the wrapped command and raises it, because
// running as another user (usually root) removes every OS-level guard rail.
func classifyPrivileged(name string, args []string, depth int) Classification {
	inner, script := splitPrivileged(name, args)

	var c Classification
	switch {
	case script != "":
		c = classifyCommandLine(script, depth+1)
	case len(inner) > 0:
		c = classifyArgv(inner, depth+1)
		c = escalateForArguments(c, inner, strings.Join(inner, " "))
	default:
		return cls(CatShellExecute, RiskHigh,
			"opens an interactive root shell via "+name, name)
	}
	c.Risk = MaxRisk(EscalateRisk(c.Risk), RiskHigh)
	c.Reason = "runs with elevated privileges (" + name + "): " + c.Reason
	if c.Matched == "" {
		c.Matched = name
	}
	return c
}

// privilegeValueFlags lists flags that consume the following argument, per
// escalation tool, so the wrapped command can be found.
var privilegeValueFlags = map[string]map[string]struct{}{
	"sudo":   words("-u", "--user", "-g", "--group", "-p", "--prompt", "-C", "--close-from", "-U", "--other-user", "-h", "--host", "-D", "--chdir", "-R", "--chroot", "-T", "--command-timeout"),
	"doas":   words("-u", "-C", "-a"),
	"pkexec": words("--user"),
	"su":     words("-s", "--shell", "-m", "--group", "-g", "--supp-group", "-G"),
	"runas":  words("/user"),
	"gosu":   words(),
}

// splitPrivileged separates the escalation wrapper from what it runs. It
// returns either the wrapped argv or, for `su -c`, the inline script.
func splitPrivileged(name string, args []string) (inner []string, script string) {
	valueFlags := privilegeValueFlags[name]
	sawUser := name != "su" && name != "runas" && name != "gosu"
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return args[i+1:], ""
		case (a == "-c" || a == "--command") && (name == "su" || name == "doas"):
			if i+1 < len(args) {
				return nil, args[i+1]
			}
			return nil, ""
		case strings.HasPrefix(a, "-") || strings.HasPrefix(a, "/"):
			if containsWord(valueFlags, a) {
				i++
			}
		case !sawUser:
			// For su/runas/gosu the first positional is the target user.
			sawUser = true
		default:
			return args[i:], ""
		}
	}
	return nil, ""
}

// classifyShellInterpreter looks through `sh -c "..."` so the real work is
// classified rather than the shell that hosts it.
func classifyShellInterpreter(name string, args []string, depth int) Classification {
	for i, a := range args {
		if (a == "-c" || a == "-Command" || a == "/c" || a == "/C" || a == "-EncodedCommand") && i+1 < len(args) {
			if a == "-EncodedCommand" {
				return cls(CatShellExecute, RiskCritical,
					"runs a base64-encoded shell command that cannot be inspected", a)
			}
			c := classifyCommandLine(args[i+1], depth+1)
			c.Reason = "runs a nested " + name + " command: " + c.Reason
			return c
		}
	}
	if len(positionals(args)) > 0 {
		return cls(CatShellExecute, RiskHigh,
			"executes a shell script whose contents Boop cannot inspect", name)
	}
	return cls(CatShellExecute, RiskMedium, "starts an interactive "+name+" shell", name)
}

// classifyWrapper classifies whatever a transparent wrapper runs.
func classifyWrapper(name string, args []string, depth int) Classification {
	rest := args
	switch name {
	case "timeout":
		// Skip flags, then the duration.
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
			rest = rest[1:]
		}
		if len(rest) > 0 {
			rest = rest[1:]
		}
	case "nice", "ionice", "chrt", "taskset", "stdbuf", "xargs", "watch", "strace", "ltrace":
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
			// Flags that take a separate value.
			if rest[0] == "-n" || rest[0] == "-c" || rest[0] == "-I" || rest[0] == "-p" ||
				rest[0] == "-o" || rest[0] == "-e" || rest[0] == "-P" {
				if len(rest) > 1 {
					rest = rest[1:]
				}
			}
			rest = rest[1:]
		}
	}
	rest = stripEnvAssignments(rest)
	if len(rest) == 0 {
		return cls(CatShellExecute, RiskMedium, "runs "+name+" with no inspectable command", name)
	}
	c := classifyArgv(rest, depth+1)
	c = escalateForArguments(c, rest, strings.Join(rest, " "))
	c.Reason = "via " + name + ": " + c.Reason
	return c
}

// dangerousTargets are paths whose recursive removal is catastrophic.
var dangerousTargets = words("/", "/*", "~", "~/", "$HOME", "$HOME/", ".", "./",
	"..", "../", "*", "/home", "/home/", "c:/", "c:\\")

// classifyDestructive covers commands whose purpose is destroying data.
func classifyDestructive(name string, args []string) (Classification, bool) {
	switch {
	case name == "rm" || name == "del" || name == "erase":
		recursive := hasShortFlag(args, 'r') || hasShortFlag(args, 'R') ||
			hasFlag(args, "--recursive", "/s", "/S")
		force := hasShortFlag(args, 'f') || hasFlag(args, "--force", "/f", "/F")
		for _, p := range positionals(args) {
			if containsWord(dangerousTargets, strings.ToLower(p)) {
				return cls(CatFilesystemWrite, RiskCritical,
					"deletes "+p+", which would destroy the filesystem or home directory", p), true
			}
		}
		switch {
		case recursive && force:
			return cls(CatFilesystemWrite, RiskCritical,
				"recursively and forcibly deletes files (rm -rf)", "rm -rf"), true
		case recursive:
			return cls(CatFilesystemWrite, RiskHigh, "recursively deletes a directory tree", "rm -r"), true
		default:
			return cls(CatFilesystemWrite, RiskMedium, "deletes files", name), true
		}

	case name == "shred" || name == "srm" || name == "wipe":
		return cls(CatFilesystemWrite, RiskCritical,
			"irreversibly overwrites file contents", name), true

	case name == "dd":
		for _, a := range args {
			if strings.HasPrefix(a, "of=") {
				return cls(CatFilesystemWrite, RiskCritical,
					"writes a raw image over "+strings.TrimPrefix(a, "of=")+", destroying whatever is there", a), true
			}
		}
		return cls(CatFilesystemWrite, RiskHigh, "performs a raw block copy", name), true

	case strings.HasPrefix(name, "mkfs"):
		return cls(CatFilesystemWrite, RiskCritical,
			"creates a new filesystem, erasing all existing data on the target", name), true

	case containsWord(partitionTools, name):
		return cls(CatFilesystemWrite, RiskCritical,
			"rewrites the partition table or wipes filesystem signatures", name), true

	case name == "truncate":
		return cls(CatFilesystemWrite, RiskHigh, "truncates files in place", name), true

	case containsWord(powerCommands, name):
		return cls(CatShellExecute, RiskCritical,
			"powers off or reboots the machine, interrupting everything running on it", name), true
	}
	return Classification{}, false
}

var (
	partitionTools = words("fdisk", "sfdisk", "cfdisk", "gdisk", "sgdisk",
		"cgdisk", "parted", "partprobe", "wipefs", "blkdiscard", "badblocks",
		"diskpart", "format")

	powerCommands = words("shutdown", "reboot", "poweroff", "halt", "init")

	// storageTools reconfigure block devices, volumes and mounts. These are
	// legitimate, frequent administrative tasks - a user may genuinely ask
	// Boop to build an LVM volume and mount it - so they are classified
	// accurately as critical shell work rather than mistaken for production
	// deployment or blocked outright.
	storageTools = words(
		"pvcreate", "pvremove", "pvresize", "pvmove", "pvchange",
		"vgcreate", "vgremove", "vgextend", "vgreduce", "vgchange", "vgrename",
		"lvcreate", "lvremove", "lvextend", "lvreduce", "lvresize", "lvchange",
		"lvrename", "lvconvert",
		"mount", "umount", "mkswap", "swapon", "swapoff", "cryptsetup",
		"losetup", "tune2fs", "resize2fs", "e2fsck", "fsck", "xfs_growfs",
		"xfs_repair", "btrfs", "zpool", "zfs", "mdadm", "multipath",
		"nvme", "hdparm", "sdparm", "smartctl", "vgs", "lvs", "pvs")

	// storageReadOnly are the inspection subset of the storage tools.
	storageReadOnly = words("vgs", "lvs", "pvs", "smartctl", "vgdisplay",
		"lvdisplay", "pvdisplay")

	blockDeviceRe = regexp.MustCompile(`^/dev/(sd[a-z]|nvme\d|vd[a-z]|hd[a-z]|xvd[a-z]|md\d|sr\d|loop\d|mapper/|disk/)`)
)

// classifyStorage covers block devices, volume management and mounts.
func classifyStorage(name string, args []string) (Classification, bool) {
	if !containsWord(storageTools, name) {
		return Classification{}, false
	}
	if containsWord(storageReadOnly, name) {
		return cls(CatShellExecute, RiskLow, "reports storage configuration", name), true
	}
	switch name {
	case "mount":
		if len(positionals(args)) == 0 && !hasFlag(args, "-a", "--all") {
			return cls(CatShellExecute, RiskLow, "lists mounted filesystems", name), true
		}
		return cls(CatShellExecute, RiskCritical,
			"mounts a filesystem, changing what the machine exposes at that path", name), true
	case "umount":
		return cls(CatShellExecute, RiskCritical,
			"unmounts a filesystem, cutting off access for anything using it", name), true
	case "cryptsetup":
		return cls(CatShellExecute, RiskCritical,
			"changes disk encryption, which can permanently lock or destroy data", name), true
	}
	return cls(CatShellExecute, RiskCritical,
		"changes block-device or volume configuration ("+name+"), which can destroy data on disk", name), true
}

// ---------------------------------------------------------------------------
// Git
// ---------------------------------------------------------------------------

var (
	gitReadSubcommands = words("status", "log", "diff", "show", "branch",
		"remote", "ls-files", "ls-remote", "ls-tree", "rev-parse", "describe",
		"blame", "shortlog", "config", "cat-file", "for-each-ref", "grep",
		"whatchanged", "reflog", "bisect", "help", "version", "fetch",
		"worktree", "stash", "notes", "count-objects", "verify-commit")

	// protectedBranches are release-bearing branches; pushing to them is
	// treated as production-affecting (§15).
	protectedBranches = words("main", "master", "production", "prod", "release",
		"stable", "live")
)

// stripGitGlobalOptions removes options that precede the subcommand, such as
// `git -c user.name=x commit`, so the subcommand can be identified.
func stripGitGlobalOptions(args []string) []string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "-C" || a == "--git-dir" || a == "--work-tree" ||
			a == "--namespace" || a == "--exec-path" || a == "--config-env":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return args[i:]
		}
	}
	return nil
}

// classifyGit classifies a git invocation. Read-only porcelain is low risk;
// history rewriting and pushes to release branches are not.
func classifyGit(args []string) Classification {
	rest := stripGitGlobalOptions(args)
	sub := strings.ToLower(firstPositional(rest))

	switch sub {
	case "push":
		force := hasFlag(rest, "--force", "-f", "--force-with-lease", "--mirror") ||
			hasAnyPrefix(rest, "--force-with-lease=")
		protected := ""
		for _, p := range positionals(rest)[1:] {
			ref := p
			if i := strings.LastIndex(ref, ":"); i >= 0 {
				ref = ref[i+1:]
			}
			ref = strings.TrimPrefix(ref, "refs/heads/")
			if containsWord(protectedBranches, strings.ToLower(ref)) ||
				strings.HasPrefix(strings.ToLower(ref), "release/") {
				protected = ref
				break
			}
		}
		switch {
		case force && protected != "":
			c := prodCls(CatGitPush, RiskCritical,
				"force-pushes to the protected branch "+protected+", which can permanently discard published history", "git push --force")
			return c
		case force:
			return cls(CatGitPush, RiskHigh,
				"force-pushes, which can permanently discard published history", "git push --force")
		case protected != "":
			c := prodCls(CatGitPush, RiskHigh,
				"pushes to the protected branch "+protected, "git push "+protected)
			return c
		default:
			return cls(CatGitPush, RiskMedium, "pushes commits to a remote repository", "git push")
		}

	case "commit":
		return cls(CatGitCommit, RiskMedium, "records a commit in the local repository", "git commit")

	case "tag":
		if hasFlag(rest, "-d", "--delete") {
			return cls(CatGitPush, RiskHigh, "deletes a git tag, which may be a published release marker", "git tag -d")
		}
		return cls(CatGitCommit, RiskMedium, "creates or lists tags", "git tag")

	case "reset":
		if hasFlag(rest, "--hard") {
			return cls(CatFilesystemWrite, RiskHigh,
				"hard-resets the working tree, discarding uncommitted changes", "git reset --hard")
		}
		return cls(CatFilesystemWrite, RiskMedium, "moves the branch pointer", "git reset")

	case "clean":
		if hasShortFlag(rest, 'f') {
			return cls(CatFilesystemWrite, RiskHigh,
				"deletes untracked files, which git cannot recover", "git clean -f")
		}
		return cls(CatGitRead, RiskLow, "lists files git clean would remove", "git clean -n")

	case "checkout", "switch", "restore":
		if hasShortFlag(rest, 'f') || hasFlag(rest, "--force", "--hard") {
			return cls(CatFilesystemWrite, RiskHigh,
				"forcibly overwrites working-tree files", "git "+sub)
		}
		return cls(CatFilesystemWrite, RiskMedium, "changes the working tree", "git "+sub)

	case "rebase", "filter-branch", "filter-repo", "cherry-pick", "revert", "merge", "am", "apply":
		return cls(CatFilesystemWrite, RiskHigh, "rewrites or replays history in the working tree", "git "+sub)

	case "add", "mv", "rm", "stage", "submodule", "init", "clone", "pull":
		if sub == "rm" {
			return cls(CatFilesystemWrite, RiskMedium, "removes tracked files", "git rm")
		}
		return cls(CatFilesystemWrite, RiskMedium, "modifies the repository working tree", "git "+sub)
	}

	if containsWord(gitReadSubcommands, sub) {
		if sub == "config" && !hasFlag(rest, "--get", "--get-all", "--list", "-l") {
			return cls(CatFilesystemWrite, RiskMedium, "changes git configuration", "git config")
		}
		return cls(CatGitRead, RiskLow, "reads repository state (git "+sub+")", "git "+sub)
	}
	if sub == "" {
		return cls(CatGitRead, RiskLow, "prints git usage", "git")
	}
	return cls(CatShellExecute, RiskMedium, "runs an unrecognised git subcommand ("+sub+")", "git "+sub)
}

// ---------------------------------------------------------------------------
// Deployment and production infrastructure
// ---------------------------------------------------------------------------

var (
	kubectlMutating = words("apply", "create", "delete", "patch", "replace",
		"edit", "scale", "rollout", "drain", "cordon", "uncordon", "taint",
		"annotate", "label", "set", "exec", "attach", "port-forward", "cp",
		"expose", "autoscale", "run", "proxy")

	helmMutating = words("install", "upgrade", "uninstall", "delete", "rollback",
		"push", "repo")

	terraformMutating = words("apply", "destroy", "import", "taint", "untaint",
		"state", "refresh", "force-unlock", "workspace")

	systemctlMutating = words("start", "stop", "restart", "reload", "enable",
		"disable", "mask", "unmask", "kill", "isolate", "daemon-reload",
		"daemon-reexec", "edit", "set-property", "reset-failed", "link",
		"revert", "preset")

	systemctlPower = words("poweroff", "reboot", "halt", "suspend", "hibernate",
		"kexec", "emergency", "rescue")
)

// classifyDeployment covers commands that reach beyond the local workspace
// into orchestrated or remote infrastructure.
func classifyDeployment(name string, args []string, depth int) (Classification, bool) {
	pos := positionals(args)
	sub := ""
	if len(pos) > 0 {
		sub = strings.ToLower(pos[0])
	}

	switch name {
	case "kubectl", "oc", "k9s", "kubeadm":
		if containsWord(kubectlMutating, sub) || name == "kubeadm" {
			return prodCls(CatProductionChange, RiskCritical,
				"changes a live Kubernetes cluster (kubectl "+sub+")", name+" "+sub), true
		}
		return prodCls(CatProductionChange, RiskHigh,
			"talks to a live Kubernetes cluster (kubectl "+sub+")", name+" "+sub), true

	case "helm":
		if containsWord(helmMutating, sub) {
			return prodCls(CatProductionChange, RiskCritical,
				"installs or removes a Helm release in a live cluster", "helm "+sub), true
		}
		return prodCls(CatProductionChange, RiskHigh, "queries a live Helm installation", "helm "+sub), true

	case "terraform", "tofu", "opentofu", "pulumi":
		switch {
		case containsWord(terraformMutating, sub) || sub == "up" || sub == "destroy":
			return prodCls(CatProductionChange, RiskCritical,
				"applies infrastructure changes ("+name+" "+sub+"), which can create or destroy real resources", name+" "+sub), true
		case sub == "plan" || sub == "init" || sub == "preview" || sub == "output" || sub == "show":
			return prodCls(CatProductionChange, RiskHigh,
				"reads live infrastructure state ("+name+" "+sub+")", name+" "+sub), true
		default:
			return cls(CatShellExecute, RiskLow, name+" "+sub+" only inspects local configuration", name+" "+sub), true
		}

	case "ansible", "ansible-playbook", "salt", "salt-call", "puppet", "chef-client":
		return prodCls(CatProductionChange, RiskCritical,
			"runs configuration management against managed hosts", name), true

	case "docker", "podman", "nerdctl":
		return classifyContainer(name, sub, args), true

	case "docker-compose":
		return classifyContainer("docker", "compose", args), true

	case "systemctl":
		switch {
		case containsWord(systemctlPower, sub):
			return cls(CatShellExecute, RiskCritical,
				"powers off or reboots the machine via systemctl", "systemctl "+sub), true
		case containsWord(systemctlMutating, sub):
			return prodCls(CatProductionChange, RiskHigh,
				"changes the state of system service(s) (systemctl "+sub+")", "systemctl "+sub), true
		default:
			return cls(CatShellExecute, RiskLow, "reads systemd state (systemctl "+sub+")", "systemctl "+sub), true
		}

	case "service", "rc-service", "sv", "launchctl", "sc":
		// `service <unit> <verb>` puts the verb second; the others put it first.
		verbs := pos
		if name == "service" || name == "rc-service" {
			if len(pos) > 1 {
				verbs = pos[1:]
			} else {
				verbs = nil
			}
		}
		for _, v := range verbs {
			if containsWord(systemctlMutating, strings.ToLower(v)) {
				return prodCls(CatProductionChange, RiskHigh,
					"changes the state of a system service ("+name+" "+v+")", name+" "+v), true
			}
		}
		if len(verbs) == 0 {
			return cls(CatShellExecute, RiskLow, "lists services", name), true
		}
		return cls(CatShellExecute, RiskMedium, "queries a system service", name+" "+sub), true

	case "ssh", "mosh", "telnet":
		return classifyRemoteShell(name, args, depth), true

	case "scp", "sftp", "rsync":
		return classifyTransfer(name, args), true

	case "crontab", "at", "systemd-run", "schtasks":
		return cls(CatShellExecute, RiskHigh,
			"schedules work that will run outside this session", name), true

	case "nc", "ncat", "netcat", "socat":
		return cls(CatNetworkHTTP, RiskHigh,
			"opens a raw network connection, which can move data off this machine or expose a shell", name), true
	}
	return Classification{}, false
}

// classifyContainer separates container reads from container mutations, and
// treats registry pushes as production changes.
func classifyContainer(name, sub string, args []string) Classification {
	switch sub {
	case "push":
		return prodCls(CatProductionChange, RiskHigh,
			"publishes an image to a registry other systems will pull", name+" push")
	case "run", "create", "start", "exec":
		if hasFlag(args, "--privileged", "--pid=host", "--net=host", "--network=host") || mountsHostRoot(args) {
			return cls(CatShellExecute, RiskCritical,
				"runs a container with host-level access", name+" "+sub)
		}
		return cls(CatShellExecute, RiskHigh, "runs a container", name+" "+sub)
	case "rm", "rmi", "kill", "stop", "prune", "system", "volume", "network":
		return cls(CatShellExecute, RiskHigh, "removes or reconfigures container resources", name+" "+sub)
	case "compose", "stack", "swarm", "build", "commit", "tag", "load", "import", "save", "pull", "login":
		return cls(CatShellExecute, RiskMedium, "builds or fetches container images", name+" "+sub)
	case "ps", "images", "logs", "inspect", "version", "info", "stats", "top", "port", "diff", "history", "":
		return cls(CatShellExecute, RiskLow, "reads container state", name+" "+sub)
	}
	return cls(CatShellExecute, RiskMedium, "runs "+name+" "+sub, name+" "+sub)
}

// mountsHostRoot reports whether a container invocation binds a sensitive host
// location into the container, which makes the container isolation cosmetic.
func mountsHostRoot(args []string) bool {
	for i, a := range args {
		v := ""
		switch {
		case a == "-v" || a == "--volume" || a == "--mount":
			if i+1 < len(args) {
				v = args[i+1]
			}
		case strings.HasPrefix(a, "--volume="), strings.HasPrefix(a, "--mount="):
			v = a[strings.Index(a, "=")+1:]
		case strings.HasPrefix(a, "-v") && len(a) > 2:
			v = a[2:]
		}
		if v == "" {
			continue
		}
		if v == "/" || strings.HasPrefix(v, "/:") || strings.Contains(v, "src=/,") ||
			strings.HasPrefix(v, "/etc") || strings.HasPrefix(v, "/root") ||
			strings.HasPrefix(v, "/var/run/docker.sock") {
			return true
		}
	}
	return false
}

// sshValueFlags consume the following argument.
var sshValueFlags = words("-p", "-i", "-l", "-o", "-F", "-L", "-R", "-D", "-b",
	"-c", "-m", "-w", "-J", "-e", "-Q", "-S", "-W", "-B", "-E", "-I")

// classifyRemoteShell treats any ssh session as production-affecting: Boop
// cannot know that the far end is a scratch box, and §15 requires the
// cautious assumption.
func classifyRemoteShell(name string, args []string, depth int) Classification {
	host := ""
	var remote []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if containsWord(sshValueFlags, a) {
				i++
			}
			continue
		}
		host = a
		remote = args[i+1:]
		break
	}
	if host == "" {
		return prodCls(CatProductionChange, RiskHigh, "opens a remote shell session", name)
	}
	c := prodCls(CatProductionChange, RiskHigh,
		"runs commands on the remote host "+host, name+" "+host)
	if len(remote) > 0 {
		inner := classifyCommandLine(strings.Join(remote, " "), depth+1)
		c.Risk = MaxRisk(c.Risk, inner.Risk)
		c.Reason = "on the remote host " + host + ": " + inner.Reason
	}
	return c
}

// classifyTransfer classifies file transfer tools, distinguishing a local
// copy from one that crosses a machine boundary.
func classifyTransfer(name string, args []string) Classification {
	for _, p := range positionals(args) {
		if isRemoteTarget(p) {
			return prodCls(CatProductionChange, RiskHigh,
				"transfers files to or from the remote target "+p, name+" "+p)
		}
	}
	if hasFlag(args, "--delete", "--delete-after", "--delete-excluded") {
		return cls(CatFilesystemWrite, RiskHigh,
			"mirrors a directory and deletes files missing from the source", name+" --delete")
	}
	return cls(CatFilesystemWrite, RiskMedium, "copies files locally", name)
}

var remoteHostRe = regexp.MustCompile(`^(?:[A-Za-z0-9._%+-]+@)?[A-Za-z0-9][A-Za-z0-9._-]*$`)

// isRemoteTarget reports whether an argument names a host:path destination.
// A single-letter prefix is treated as a Windows drive, not a hostname.
func isRemoteTarget(s string) bool {
	if strings.HasPrefix(s, "rsync://") || strings.HasPrefix(s, "ssh://") {
		return true
	}
	if strings.Contains(s, "://") {
		return false
	}
	i := strings.Index(s, ":")
	if i <= 1 {
		return false
	}
	host := s[:i]
	if strings.ContainsAny(host, "/\\") {
		return false
	}
	return remoteHostRe.MatchString(host)
}

// ---------------------------------------------------------------------------
// Cloud CLIs
// ---------------------------------------------------------------------------

var (
	// cloudReadVerbs are the verbs known to be non-mutating. Anything not on
	// this list is assumed to mutate, because guessing wrong in the other
	// direction hands the model a silent production change.
	cloudReadVerbs = words("describe", "list", "get", "ls", "show", "head",
		"status", "version", "help", "info", "config", "search", "test",
		"validate", "check", "wait", "estimate", "print", "read")

	cloudMutatingPrefixes = []string{"create", "delete", "put", "update",
		"modify", "terminate", "reboot", "run", "attach", "detach", "associate",
		"disassociate", "start", "stop", "remove", "set", "add", "deploy",
		"apply", "destroy", "import", "restore", "revoke", "authorize",
		"enable", "disable", "kill", "cancel", "purge", "sync", "rm", "mv",
		"cp", "rb", "mb", "register", "deregister", "publish", "send",
		"invoke", "scale", "rotate", "reset", "replace", "promote"}
)

// classifyCloud covers the major cloud CLIs. Mutating verbs are production
// changes; read verbs still reach a real account, so they are not free.
func classifyCloud(name string, args []string) (Classification, bool) {
	switch name {
	case "aws", "gcloud", "gsutil", "az", "doctl", "linode-cli", "hcloud",
		"eksctl", "flyctl", "fly", "heroku", "vercel", "netlify", "wrangler":
	default:
		return Classification{}, false
	}

	pos := positionals(args)
	verb := ""
	mutating := false
	for _, p := range pos {
		low := strings.ToLower(p)
		if containsWord(cloudReadVerbs, low) {
			if verb == "" {
				verb = low
			}
			continue
		}
		for _, prefix := range cloudMutatingPrefixes {
			if low == prefix || strings.HasPrefix(low, prefix+"-") {
				verb, mutating = low, true
				break
			}
		}
		if mutating {
			break
		}
	}
	switch {
	case mutating:
		return prodCls(CatProductionChange, RiskCritical,
			"changes cloud resources ("+name+" "+verb+"), which may affect live systems and billing", name+" "+verb), true
	case verb != "":
		return cls(CatNetworkHTTP, RiskMedium,
			"reads cloud account state ("+name+" "+verb+")", name+" "+verb), true
	default:
		return prodCls(CatProductionChange, RiskCritical,
			"runs an unrecognised "+name+" command, which is assumed to change cloud resources", name), true
	}
}

// ---------------------------------------------------------------------------
// Network configuration
// ---------------------------------------------------------------------------

var (
	firewallTools = words("iptables", "ip6tables", "iptables-restore",
		"nft", "nftables", "ufw", "firewall-cmd", "pfctl", "ipfw", "netsh")

	ipMutatingVerbs = words("add", "del", "delete", "change", "replace",
		"flush", "set", "append", "up", "down")
)

// classifyNetworkConfig covers firewall and routing changes. These are
// production-adjacent: a wrong rule can cut off access to the machine and,
// per §15, firewall rules are explicitly production-affecting.
func classifyNetworkConfig(name string, args []string) (Classification, bool) {
	pos := positionals(args)
	sub := ""
	if len(pos) > 0 {
		sub = strings.ToLower(pos[0])
	}

	switch {
	case containsWord(firewallTools, name):
		readOnly := false
		switch name {
		case "iptables", "ip6tables":
			readOnly = (hasFlag(args, "-L", "--list", "-S", "--list-rules", "-n", "-V", "--version")) &&
				!hasFlag(args, "-A", "-I", "-D", "-F", "-P", "-X", "-N", "-R", "-Z")
		case "ufw":
			readOnly = sub == "status" || sub == "show" || sub == "version"
		case "nft", "nftables":
			readOnly = sub == "list"
		case "firewall-cmd":
			readOnly = hasAnyPrefix(args, "--list", "--get", "--state", "--query") &&
				!hasAnyPrefix(args, "--add", "--remove", "--set", "--reload", "--change")
		case "pfctl":
			readOnly = hasFlag(args, "-s", "-sr")
		}
		if readOnly {
			return cls(CatShellExecute, RiskMedium, "reads the firewall configuration", name), true
		}
		return prodCls(CatProductionChange, RiskHigh,
			"changes firewall rules, which can cut off access to this machine and its services", name), true

	case name == "netplan":
		if sub == "get" || sub == "status" || sub == "info" {
			return cls(CatShellExecute, RiskMedium, "reads the network configuration", "netplan "+sub), true
		}
		return prodCls(CatProductionChange, RiskHigh,
			"applies a new network configuration, which can drop connectivity to this machine", "netplan "+sub), true

	case name == "ip":
		for _, p := range pos {
			if containsWord(ipMutatingVerbs, strings.ToLower(p)) {
				return prodCls(CatProductionChange, RiskHigh,
					"changes network addressing or routing, which can drop connectivity to this machine", "ip "+p), true
			}
		}
		return cls(CatShellExecute, RiskLow, "reads network configuration", "ip "+sub), true

	case name == "route" || name == "ifconfig" || name == "nmcli" || name == "networksetup":
		if len(pos) == 0 || sub == "show" || sub == "status" || sub == "-n" {
			return cls(CatShellExecute, RiskLow, "reads network configuration", name), true
		}
		return prodCls(CatProductionChange, RiskHigh,
			"changes network configuration, which can drop connectivity to this machine", name+" "+sub), true

	case name == "hostnamectl" || name == "timedatectl" || name == "sysctl":
		if len(pos) == 0 || hasFlag(args, "-a", "--all") || sub == "status" || sub == "show" {
			return cls(CatShellExecute, RiskLow, "reads system settings", name), true
		}
		return cls(CatShellExecute, RiskHigh, "changes kernel or host settings", name+" "+sub), true
	}
	return Classification{}, false
}

// ---------------------------------------------------------------------------
// Users, permissions and credentials
// ---------------------------------------------------------------------------

var accountTools = words("useradd", "userdel", "usermod", "adduser", "deluser",
	"groupadd", "groupdel", "groupmod", "addgroup", "delgroup", "passwd",
	"chpasswd", "gpasswd", "chage", "visudo", "vipw", "vigr", "newusers",
	"net", "dscl")

// classifyAccounts covers ownership, permission and account changes.
func classifyAccounts(name string, args []string) (Classification, bool) {
	switch {
	case containsWord(accountTools, name):
		return cls(CatShellExecute, RiskHigh,
			"changes user accounts or credentials on this machine", name), true

	case name == "chmod":
		mode := firstPositional(args)
		recursive := hasShortFlag(args, 'R') || hasFlag(args, "--recursive")
		worldWritable := strings.HasSuffix(mode, "777") || strings.HasSuffix(mode, "666") ||
			strings.Contains(mode, "o+w") || strings.Contains(mode, "a+w") ||
			strings.HasSuffix(mode, "4755") || strings.Contains(mode, "u+s")
		switch {
		case worldWritable:
			return cls(CatFilesystemWrite, RiskHigh,
				"grants world-writable or setuid permissions ("+mode+")", "chmod "+mode), true
		case recursive:
			return cls(CatFilesystemWrite, RiskHigh, "recursively changes file permissions", "chmod -R"), true
		default:
			return cls(CatFilesystemWrite, RiskMedium, "changes file permissions", "chmod"), true
		}

	case name == "chown" || name == "chgrp":
		if hasShortFlag(args, 'R') || hasFlag(args, "--recursive") {
			return cls(CatFilesystemWrite, RiskHigh,
				"recursively changes file ownership", name+" -R"), true
		}
		return cls(CatFilesystemWrite, RiskMedium, "changes file ownership", name), true

	case name == "setfacl" || name == "chattr" || name == "icacls" || name == "takeown":
		return cls(CatFilesystemWrite, RiskHigh, "changes filesystem access control", name), true

	case name == "ssh-keygen" || name == "gpg" || name == "openssl" || name == "keytool":
		return cls(CatFilesystemWrite, RiskHigh,
			"handles key material, which may create or overwrite credentials", name), true
	}
	return Classification{}, false
}

// ---------------------------------------------------------------------------
// Package managers
// ---------------------------------------------------------------------------

var (
	systemPackageManagers = words("apt", "apt-get", "aptitude", "dpkg", "yum",
		"dnf", "rpm", "zypper", "pacman", "apk", "emerge", "snap", "flatpak",
		"brew", "port", "choco", "winget", "scoop", "nix-env")

	packageMutatingVerbs = words("install", "reinstall", "remove", "purge",
		"uninstall", "upgrade", "dist-upgrade", "full-upgrade", "autoremove",
		"erase", "add", "del", "update", "refresh", "-s", "-r", "-u", "-i",
		"tap", "link", "unlink")

	packageReadVerbs = words("list", "search", "show", "info", "policy",
		"madison", "which", "outdated", "deps", "config", "--version")
)

// classifyPackages covers software installation. A global install runs
// arbitrary maintainer scripts as root and changes the machine for every
// project on it, so it is high risk even though it looks routine.
func classifyPackages(name string, args []string) (Classification, bool) {
	pos := positionals(args)
	sub := ""
	if len(pos) > 0 {
		sub = strings.ToLower(pos[0])
	}

	switch {
	case containsWord(systemPackageManagers, name):
		if containsWord(packageReadVerbs, sub) {
			return cls(CatShellExecute, RiskLow, "queries the package database", name+" "+sub), true
		}
		if name == "pacman" && hasAnyPrefix(args, "-S", "-R", "-U") ||
			containsWord(packageMutatingVerbs, sub) || sub == "" {
			return cls(CatShellExecute, RiskHigh,
				"installs or removes system-wide packages, changing this machine for every project on it", name+" "+sub), true
		}
		return cls(CatShellExecute, RiskHigh, "runs a system package manager", name+" "+sub), true

	case name == "npm" || name == "yarn" || name == "pnpm":
		global := hasFlag(args, "-g", "--global") || hasShortFlag(args, 'g')
		switch {
		case sub == "publish":
			return prodCls(CatProductionChange, RiskHigh,
				"publishes a package to a public registry, which cannot be fully undone", name+" publish"), true
		case (sub == "install" || sub == "i" || sub == "add" || sub == "ci" || sub == "up" || sub == "upgrade") && global:
			return cls(CatShellExecute, RiskHigh,
				"installs a package globally, changing this machine for every project on it", name+" -g"), true
		case sub == "install" || sub == "i" || sub == "add" || sub == "ci":
			return cls(CatShellExecute, RiskMedium,
				"installs dependencies, which runs package lifecycle scripts", name+" "+sub), true
		case sub == "test" || sub == "run" && len(pos) > 1 && isTestScript(pos[1]):
			return cls(CatShellExecute, RiskLow, "runs the project test script", name+" "+sub), true
		case sub == "run" || sub == "exec" || sub == "start":
			return cls(CatShellExecute, RiskMedium, "runs a project script", name+" "+sub), true
		default:
			return cls(CatShellExecute, RiskLow, "runs "+name+" "+sub, name+" "+sub), true
		}

	case name == "npx" || name == "pnpx" || name == "bunx" || name == "uvx":
		return cls(CatShellExecute, RiskHigh,
			"downloads and executes a package that is not part of this project", name), true

	case name == "pip" || name == "pip3" || name == "pipx" || name == "uv" || name == "poetry":
		switch sub {
		case "install", "add", "sync":
			if hasFlag(args, "--user") {
				return cls(CatShellExecute, RiskMedium,
					"installs Python packages into the user site directory", name+" install --user"), true
			}
			return cls(CatShellExecute, RiskHigh,
				"installs Python packages, which runs setup code and may change the system interpreter", name+" "+sub), true
		case "uninstall", "remove":
			return cls(CatShellExecute, RiskHigh, "removes Python packages", name+" "+sub), true
		default:
			return cls(CatShellExecute, RiskLow, "runs "+name+" "+sub, name+" "+sub), true
		}

	case name == "gem" || name == "composer" || name == "nuget":
		if sub == "install" || sub == "update" || sub == "uninstall" {
			return cls(CatShellExecute, RiskHigh, "installs or removes packages system-wide", name+" "+sub), true
		}
		return cls(CatShellExecute, RiskLow, "runs "+name+" "+sub, name+" "+sub), true
	}
	return Classification{}, false
}

func isTestScript(s string) bool {
	switch strings.ToLower(s) {
	case "test", "tests", "unit", "lint", "typecheck", "check", "coverage":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Ordinary development commands
// ---------------------------------------------------------------------------

var (
	goLowRisk     = words("build", "test", "vet", "fmt", "list", "doc", "version", "env", "tool")
	goMediumRisk  = words("run", "generate", "install", "get", "mod", "work", "clean")
	makeHighRisk  = words("install", "uninstall", "deploy", "publish", "release", "dist")
	cargoLowRisk  = words("build", "test", "check", "fmt", "clippy", "doc", "tree", "metadata")
	inlineCodeArg = words("-c", "-e", "--eval", "--command", "-E")
)

// classifyOrdinary covers everyday reads, builds and tests: the work that
// should not interrupt the user.
func classifyOrdinary(name string, args []string, depth int) (Classification, bool) {
	pos := positionals(args)
	sub := ""
	if len(pos) > 0 {
		sub = strings.ToLower(pos[0])
	}

	switch name {
	case "find", "fd":
		if hasFlag(args, "-delete") {
			return cls(CatFilesystemWrite, RiskHigh, "deletes every file it matches", "find -delete"), true
		}
		for i, a := range args {
			if (a == "-exec" || a == "-execdir" || a == "-x" || a == "--exec") && i+1 < len(args) {
				inner := args[i+1:]
				for j, t := range inner {
					if t == ";" || t == "+" {
						inner = inner[:j]
						break
					}
				}
				if len(inner) > 0 {
					c := classifyArgv(inner, depth+1)
					c.Reason = "runs a command for every matched file: " + c.Reason
					c.Risk = MaxRisk(c.Risk, RiskMedium)
					return c, true
				}
			}
		}
		return cls(CatFilesystemRead, RiskLow, "searches the filesystem", name), true

	case "go":
		switch {
		case containsWord(goLowRisk, sub):
			return cls(CatShellExecute, RiskLow, "runs go "+sub, "go "+sub), true
		case containsWord(goMediumRisk, sub):
			return cls(CatShellExecute, RiskMedium, "runs go "+sub+", which builds and executes code or fetches modules", "go "+sub), true
		default:
			return cls(CatShellExecute, RiskMedium, "runs go "+sub, "go "+sub), true
		}

	case "make", "gmake", "ninja":
		for _, target := range pos {
			if containsWord(makeHighRisk, strings.ToLower(target)) {
				if target == "deploy" || target == "publish" || target == "release" {
					return prodCls(CatProductionChange, RiskHigh,
						"runs the "+target+" target, which publishes or deploys artifacts", name+" "+target), true
				}
				return cls(CatShellExecute, RiskHigh,
					"runs the "+target+" target, which installs outside the workspace", name+" "+target), true
			}
		}
		return cls(CatShellExecute, RiskLow, "runs a build target", name+" "+sub), true

	case "cargo":
		switch {
		case sub == "install":
			return cls(CatShellExecute, RiskHigh, "installs a binary outside the workspace", "cargo install"), true
		case sub == "publish":
			return prodCls(CatProductionChange, RiskHigh, "publishes a crate to the registry", "cargo publish"), true
		case containsWord(cargoLowRisk, sub):
			return cls(CatShellExecute, RiskLow, "runs cargo "+sub, "cargo "+sub), true
		default:
			return cls(CatShellExecute, RiskMedium, "runs cargo "+sub, "cargo "+sub), true
		}

	case "mvn", "gradle", "dotnet", "sbt":
		for _, target := range pos {
			switch strings.ToLower(target) {
			case "deploy", "publish", "release":
				return prodCls(CatProductionChange, RiskHigh,
					"publishes build artifacts to a shared repository", name+" "+target), true
			}
		}
		return cls(CatShellExecute, RiskLow, "runs a build target", name+" "+sub), true

	case "sed", "awk", "perl":
		if name == "sed" && (hasShortFlag(args, 'i') || hasAnyPrefix(args, "--in-place", "-i.")) {
			return cls(CatFilesystemWrite, RiskMedium, "edits files in place", "sed -i"), true
		}
		if name == "perl" {
			break // handled as a script interpreter below
		}
		return cls(CatFilesystemRead, RiskLow, "transforms text", name), true

	case "tee":
		return cls(CatFilesystemWrite, RiskMedium, "writes its input to a file", name), true

	case "cp", "mv", "ln", "install", "mkdir", "touch", "rsync-local":
		if name == "mkdir" || name == "touch" {
			return cls(CatFilesystemWrite, RiskLow, "creates files or directories", name), true
		}
		return cls(CatFilesystemWrite, RiskMedium, "copies, moves or links files", name), true

	case "kill", "killall", "pkill", "taskkill":
		return cls(CatShellExecute, RiskMedium, "terminates running processes", name), true

	case "curl", "wget", "iwr", "invoke-webrequest", "aria2c", "httpie", "http":
		if hasFlag(args, "-o", "-O", "--output", "--remote-name") {
			return cls(CatNetworkHTTP, RiskMedium, "downloads a file from the network", name), true
		}
		return cls(CatNetworkHTTP, RiskMedium, "makes a network request", name), true
	}

	if containsWord(scriptInterpreters, name) {
		for _, a := range args {
			if containsWord(inlineCodeArg, a) {
				return cls(CatShellExecute, RiskHigh,
					"executes inline "+name+" code, which Boop cannot inspect", name+" -c"), true
			}
		}
		if len(pos) > 0 {
			return cls(CatShellExecute, RiskMedium,
				"runs the "+name+" program "+pos[0], name), true
		}
		return cls(CatShellExecute, RiskMedium, "starts an interactive "+name+" session", name), true
	}
	if containsWord(readOnlyCommands, name) {
		return cls(CatFilesystemRead, RiskLow, "reads files or system state ("+name+")", name), true
	}
	if containsWord(buildCommands, name) {
		return cls(CatShellExecute, RiskLow, "builds, formats or tests code ("+name+")", name), true
	}
	return Classification{}, false
}

// ---------------------------------------------------------------------------
// Whole-segment escalations
// ---------------------------------------------------------------------------

var (
	substitutionRe = regexp.MustCompile(`\$\(|` + "`")
	// fetchSubstRe catches `bash <(curl ...)` and `eval "$(curl ...)"`, which
	// are the pipe-to-shell pattern written differently.
	fetchSubstRe = regexp.MustCompile(`[<$]\(\s*(curl|wget|iwr|invoke-webrequest|aria2c)\b`)
)

// escalateForArguments applies checks that depend on the arguments rather than
// the program: command substitution, raw block devices, redirection targets
// and paths that leave the workspace or touch credentials.
func escalateForArguments(c Classification, argv []string, raw string) Classification {
	readOnly := c.Category == CatFilesystemRead || c.Category == CatGitRead

	if substitutionRe.MatchString(raw) {
		c = raise(c, RiskHigh,
			"contains command substitution, so what it actually runs cannot be determined in advance", "$(...)")
	}
	if commandName(argv[0]) == "eval" {
		c = raise(c, RiskHigh, "evaluates a constructed command string", "eval")
	}

	for i, tok := range argv {
		if blockDeviceRe.MatchString(tok) {
			c = raise(c, RiskCritical,
				"operates directly on the block device "+tok, tok)
		}
		// A redirection makes this a write, whatever the program is - unless
		// it is discarded to a pseudo-device.
		if tok == ">" || tok == ">>" {
			if i+1 < len(argv) {
				if _, benign := benignDevices[strings.ToLower(argv[i+1])]; benign {
					continue
				}
			}
			if readOnly {
				c.Category = CatFilesystemWrite
				readOnly = false
			}
			c = raise(c, RiskMedium, "writes output to a file", tok)
			if i+1 < len(argv) {
				c = escalateForPath(c, argv[i+1], false)
			}
			continue
		}
		c = escalateForPath(c, tok, readOnly)
	}
	return c
}

// escalateForPath raises c when an argument names a system location or
// credential material. Reads of system configuration are treated more gently
// than writes, but credential material is critical either way because the
// risk there is exfiltration, not corruption.
func escalateForPath(c Classification, tok string, readOnly bool) Classification {
	if !looksLikePath(tok) {
		return c
	}
	switch {
	case isCredentialPath(tok):
		return raise(c, RiskCritical,
			"touches credential or key material ("+tok+")", tok)
	case isSystemPath(tok):
		if readOnly {
			return raise(c, RiskMedium, "reads system configuration ("+tok+")", tok)
		}
		return raise(c, RiskCritical,
			"writes to the system location "+tok+", outside any workspace", tok)
	}
	return c
}

// looksLikePath reports whether a token is an absolute or home-relative path
// worth classifying. Relative paths are left to the workspace boundary.
func looksLikePath(tok string) bool {
	switch {
	case strings.HasPrefix(tok, "/"):
		return true
	case strings.HasPrefix(tok, "~/") || tok == "~":
		return true
	case strings.HasPrefix(tok, "$HOME/") || strings.HasPrefix(tok, "${HOME}/"):
		return true
	case windowsAbsRe.MatchString(tok):
		return true
	}
	return false
}

var windowsAbsRe = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// classifyFetchExec detects the download-and-run pattern, where remote code is
// piped straight into an interpreter. Nothing about the fetched script can be
// known in advance, so it is always critical.
func classifyFetchExec(pipeline [][]string) (Classification, bool) {
	if len(pipeline) < 2 {
		return Classification{}, false
	}
	fetchedAt := -1
	for i, argv := range pipeline {
		name := commandName(argv[0])
		if containsWord(fetchers, name) {
			fetchedAt = i
			continue
		}
		if fetchedAt < 0 || i <= fetchedAt {
			continue
		}
		inner := argv
		if containsWord(privilegeEscalators, name) {
			if wrapped, _ := splitPrivileged(name, argv[1:]); len(wrapped) > 0 {
				inner = wrapped
				name = commandName(inner[0])
			}
		}
		if containsWord(shellInterpreters, name) || containsWord(scriptInterpreters, name) {
			return cls(CatShellExecute, RiskCritical,
				"downloads a remote script and executes it immediately, so its contents are unknown until they have already run",
				commandName(pipeline[fetchedAt][0])+" | "+name), true
		}
	}
	return Classification{}, false
}

// classifyProcessSubstitution catches `bash <(curl ...)`, which is the same
// download-and-run pattern spelled differently.
func classifyProcessSubstitution(cmdline string) (Classification, bool) {
	if fetchSubstRe.MatchString(cmdline) {
		return cls(CatShellExecute, RiskCritical,
			"executes a remotely fetched script through substitution, so its contents are unknown until they have already run",
			"<(curl ...)"), true
	}
	return Classification{}, false
}

// ---------------------------------------------------------------------------
// Path classification
// ---------------------------------------------------------------------------

// systemDirs are locations owned by the operating system or shared by every
// user of the machine.
var systemDirs = []string{
	"/etc", "/boot", "/sys", "/proc", "/dev", "/usr", "/bin", "/sbin",
	"/lib", "/lib32", "/lib64", "/var", "/opt", "/root", "/srv",
	"c:/windows", "c:/program files", "c:/program files (x86)", "c:/programdata",
}

// credentialDirs hold secrets by convention.
var credentialDirs = []string{
	"/.ssh/", "/.aws/", "/.gnupg/", "/.gpg/", "/.config/gh/", "/.kube/",
	"/.docker/", "/.azure/", "/.gcloud/", "/.config/gcloud/", "/.netrc",
	"/.pgpass", "/.npmrc", "/.pypirc", "/.git-credentials",
	"/etc/shadow", "/etc/sudoers", "/etc/ssl/private",
}

// credentialSuffixes match key material by file name.
var credentialSuffixes = []string{".pem", ".key", ".p12", ".pfx", ".jks",
	".keystore", ".asc", ".ppk", "id_rsa", "id_ed25519", "id_ecdsa", "id_dsa",
	"credentials.json", "service-account.json"}

// ClassifyPath returns the risk of touching path and whether it lies inside
// workspaceRoot.
//
// Ordering matters, and it is deliberately not the naive one: a path inside
// the workspace is ordinary work even when the workspace happens to live under
// a system directory such as /var/www, so containment is checked before the
// system-directory rule. Credential material is critical wherever it lives,
// including inside the workspace, because the danger there is disclosure.
//
// The check is textual: relative paths are resolved against workspaceRoot but
// symlinks are not followed. Containment that must actually hold is enforced
// by the workspace boundary in the tools layer, which resolves symlinks.
func ClassifyPath(path, workspaceRoot string) (Risk, bool) {
	if strings.TrimSpace(path) == "" {
		return RiskHigh, false
	}
	root := normalizePath(workspaceRoot, "")
	p := normalizePath(path, root)
	inside := root != "" && isWithin(p, root)

	if isCredentialPath(p) {
		return RiskCritical, inside
	}
	if inside && !isSystemRoot(root) {
		if isSensitiveWorkspacePath(p, root) {
			return RiskMedium, true
		}
		return RiskLow, true
	}
	if isSystemPath(p) {
		return RiskCritical, inside
	}
	return RiskHigh, inside
}

// ClassifyPaths returns the most severe risk across paths, and whether every
// path lies inside the workspace.
func ClassifyPaths(paths []string, workspaceRoot string) (Risk, bool) {
	worst := Risk("")
	allInside := true
	for _, p := range paths {
		r, inside := ClassifyPath(p, workspaceRoot)
		worst = MaxRisk(worst, r)
		allInside = allInside && inside
	}
	if worst == "" {
		return RiskLow, true
	}
	return worst, allInside
}

// normalizePath produces a comparable, slash-separated, cleaned path,
// expanding a leading ~ or $HOME and resolving relative paths against base.
func normalizePath(p, base string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, "\\", "/")
	switch {
	case p == "~" || strings.HasPrefix(p, "~/"):
		if home, err := os.UserHomeDir(); err == nil {
			p = strings.TrimSuffix(strings.ReplaceAll(home, "\\", "/"), "/") + strings.TrimPrefix(p, "~")
		}
	case strings.HasPrefix(p, "$HOME/") || strings.HasPrefix(p, "${HOME}/"):
		if home, err := os.UserHomeDir(); err == nil {
			rest := strings.TrimPrefix(strings.TrimPrefix(p, "${HOME}"), "$HOME")
			p = strings.TrimSuffix(strings.ReplaceAll(home, "\\", "/"), "/") + rest
		}
	}
	if !strings.HasPrefix(p, "/") && !windowsAbsRe.MatchString(p) && base != "" {
		p = strings.TrimSuffix(base, "/") + "/" + p
	}
	return cleanSlashPath(p)
}

// cleanSlashPath removes "." and ".." elements and duplicate separators
// without consulting the filesystem or the host path syntax.
func cleanSlashPath(p string) string {
	rooted := strings.HasPrefix(p, "/")
	drive := ""
	if m := windowsAbsRe.FindString(p); m != "" {
		drive = strings.ToLower(m[:2])
		p = p[2:]
		rooted = true
	}
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
		case "..":
			if len(out) > 0 && out[len(out)-1] != ".." {
				out = out[:len(out)-1]
			} else if !rooted {
				out = append(out, "..")
			}
		default:
			out = append(out, part)
		}
	}
	joined := strings.Join(out, "/")
	switch {
	case drive != "":
		return drive + "/" + joined
	case rooted:
		return "/" + joined
	case joined == "":
		return "."
	default:
		return joined
	}
}

// isWithin reports whether p is at or beneath root, on cleaned paths.
func isWithin(p, root string) bool {
	if root == "" {
		return false
	}
	if p == root {
		return true
	}
	return strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

// isSystemRoot reports whether a workspace root is itself a system location,
// in which case being "inside the workspace" earns no discount.
func isSystemRoot(root string) bool {
	if root == "" || root == "/" || root == "." {
		return true
	}
	for _, dir := range systemDirs {
		if root == dir {
			return true
		}
	}
	return false
}

// benignDevices are pseudo-devices that are safe to read or write; treating
// "> /dev/null" as a critical system write would train users to click through
// approvals, which is its own security problem.
var benignDevices = map[string]struct{}{
	"/dev/null": {}, "/dev/zero": {}, "/dev/random": {}, "/dev/urandom": {},
	"/dev/stdin": {}, "/dev/stdout": {}, "/dev/stderr": {}, "/dev/tty": {},
}

// isSystemPath reports whether p lies in an operating-system location.
func isSystemPath(p string) bool {
	low := strings.ToLower(p)
	if low == "/" {
		return true
	}
	if _, benign := benignDevices[low]; benign || strings.HasPrefix(low, "/dev/fd/") {
		return false
	}
	for _, dir := range systemDirs {
		if low == dir || strings.HasPrefix(low, dir+"/") {
			return true
		}
	}
	return false
}

// isCredentialPath reports whether p names secret or key material.
func isCredentialPath(p string) bool {
	low := strings.ToLower(strings.ReplaceAll(p, "\\", "/"))
	for _, dir := range credentialDirs {
		if strings.Contains(low+"/", dir) {
			return true
		}
	}
	base := low
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".env") {
		return true
	}
	for _, suffix := range credentialSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

// isSensitiveWorkspacePath marks in-workspace paths that deserve a second look
// even though they are inside the project: repository metadata and CI
// definitions, which execute elsewhere.
func isSensitiveWorkspacePath(p, root string) bool {
	rel := strings.TrimPrefix(strings.TrimPrefix(p, strings.TrimSuffix(root, "/")), "/")
	switch {
	case rel == ".git" || strings.HasPrefix(rel, ".git/"):
		return true
	case strings.HasPrefix(rel, ".github/workflows"), strings.HasPrefix(rel, ".gitlab-ci"):
		return true
	case strings.HasPrefix(rel, ".circleci/"), strings.HasPrefix(rel, ".husky/"):
		return true
	}
	return false
}
