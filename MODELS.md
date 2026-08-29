# Choosing a model for Boop

Boop is a **tool-driven agent runtime**. It does not ask a model to describe
what it would do — it hands the model a set of callable tools and executes what
it asks for. That single fact governs model choice more than any benchmark:

> **A model without reliable tool calling cannot drive Boop at all.**
> Everything else — parameter count, benchmark score, context window — is
> secondary to whether the model emits well-formed tool calls, consistently,
> turn after turn.

This document is written for that constraint. It covers what runs locally,
what each family is good at, which backend serves it best, and where the sharp
edges are.

---

## Contents

- [The short answer](#the-short-answer)
- [What "tool calling" actually costs](#what-tool-calling-actually-costs)
- [Measured: small models are unreliable](#measured-small-models-are-unreliable)
- [Models by job](#models-by-job)
- [Backends](#backends)
- [Sizing, quantization and VRAM](#sizing-quantization-and-vram)
- [Context windows](#context-windows)
- [Configuring Boop](#configuring-boop)
- [Troubleshooting](#troubleshooting)

---

## The short answer

| You have | Run this | Why |
|:--|:--|:--|
| 8–12 GB VRAM | **Qwen2.5-Coder 7B** (Q4_K_M) | The smallest model that is genuinely usable for tool-driven coding |
| 16 GB VRAM | **Qwen2.5-Coder 14B** or **Devstral Small 24B** (Q4) | The sweet spot; Devstral is purpose-built for agentic scaffolds |
| 24 GB (RTX 3090/4090) | **Devstral Small 24B** (Q5/Q6) or **Qwen3-Coder 30B-A3B** | Comfortable agentic coding |
| 32–64 GB unified (Mac) | **Qwen3-Coder 30B-A3B**, **GLM-4 32B** | MoE models shine here — high quality, modest active parameters |
| 64 GB+ / multi-GPU | **Qwen3-Coder** larger variants, **DeepSeek-V3** derivatives | Approaching frontier quality locally |
| CPU only | **Qwen2.5-Coder 3B/7B** (Q4) | Slow but works; expect tens of seconds per turn |
| Just chatting, no tools | **Gemma 3**, **Mistral Small**, **Llama 3.3** | Strong writers, weak or absent tool calling |

**If you want one recommendation and have the VRAM: Devstral Small or
Qwen3-Coder.** Both were designed for agent harnesses rather than chat, which
is exactly what Boop is.

---

## What "tool calling" actually costs

A single Boop turn can involve several round trips: the model asks to read a
file, gets the contents back, asks to run a test, reads the failure, edits the
file, runs the test again. Each round trip re-sends the growing conversation.

This means three things when picking a model:

1. **Consistency matters more than peak ability.** A model that emits a perfect
   tool call 70% of the time and prose the other 30% is worse in practice than
   a slightly weaker model that is right 95% of the time, because every failure
   costs a wasted round trip and may poison the context with a hallucinated
   result.
2. **Tokens add up fast.** A five-step repair loop on a medium file can be tens
   of thousands of prompt tokens. Small context windows bite sooner than you
   expect.
3. **The failure mode is silent.** A model that cannot tool-call does not error
   — it writes `{"name": "read", "parameters": {...}}` as *text* and then
   invents the result. Boop shows you the raw text, but the model may go on to
   reason confidently from data it made up.

---

## Measured: small models are unreliable

Measured against a live Ollama 0.31.2 server using Boop itself. The task was
deliberately trivial — *"use the read tool to read notes.txt, then state the
exact number of lines"* — and the count is how many of five attempts produced a
real tool invocation rather than prose or fabricated output.

| Model | Params | Proper tool calls |
|:--|:--:|:--:|
| `qwen2.5:7b` | 7.6B | 4 / 5 |
| `llama3.2` | 3.2B | 4 / 5 |
| `llama3.1:8b` | 8.0B | 3 / 5 |
| `thugsred2` (llama3.1 derived) | 8.0B | 3 / 5 |

**Five trials is indicative, not rigorous** — treat these as "roughly 60–80%",
not as precise rankings. The point is the order of magnitude: general-purpose
7–8B models fail the simplest possible tool task something like a third of the
time. One observed `llama3.1:8b` failure emitted this as message text —

```
{"name": "read", "parameters": {"lines": true, "path": "notes.txt"}}
```

— and then hallucinated a plausible-looking file listing that bore no
relationship to the actual two-line file.

**Conclusion:** a general-purpose 7–8B model is fine for trying Boop out. For
real work, use a coder-tuned or agent-tuned model, and prefer 14B+ if you can
afford it. Coder-tuned models are trained on tool-call formats; general chat
models are not.

---

## Models by job

### Agentic coding — the main job

| Family | Sizes | Notes |
|:--|:--|:--|
| **Qwen3-Coder** | 30B-A3B and larger | Currently the strongest open family for tool-calling code agents. The 30B-A3B MoE activates ~3B parameters, so it runs far lighter than its size suggests. |
| **Qwen2.5-Coder** | 1.5B / 3B / 7B / 14B / 32B | The dependable workhorse. 7B is the practical floor; 14B and 32B are markedly better. Excellent FIM support. |
| **Devstral** (Mistral) | 24B | Purpose-built for agent scaffolds rather than chat. Punches well above its size on real repository tasks and fits a single 24 GB card. |
| **GLM-4 / GLM-4.5+** | 9B – 32B+ | Strong long-context and agentic behaviour; the larger variants are competitive with much bigger models. |
| **DeepSeek-Coder-V2** | 16B-A2.4B / 236B | The Lite MoE is efficient and capable. |

### General assistant work

| Family | Notes |
|:--|:--|
| **Llama 3.1 / 3.3** | Good general writing and reasoning. Tool calling works but is inconsistent at 8B (see above); 70B is far more reliable. |
| **Mistral Small / Nemo** | Efficient, good instruction-following, solid tool support. |
| **Gemma 3** | Excellent writing quality and **natively multimodal**, but **no tool calling** — usable for `/prep`-style summarisation, not for driving Boop. |
| **Phi-4** | Strong reasoning for its size; small context. |

### Reasoning-heavy tasks

**DeepSeek-R1** and its distillations, **QwQ**, and the reasoning modes of
Qwen3 and GLM. These emit long chains of thought — Boop surfaces that through
`EventReasoning`, kept separate from the answer. Be aware reasoning tokens are
billed and counted like any other, and can dominate a turn.

### Vision

**Qwen2.5-VL**, **Llama 3.2 Vision** (11B/90B), **Gemma 3**, **MiniCPM-V**,
**LLaVA**. Note that plain `llama3.1` and plain `llama3.2` are **text-only** —
the vision variant is a separate model. Boop only attaches images to a model
reporting `CapabilityVision`, and tells you which configured models qualify
when the current one does not.

### Embeddings

**nomic-embed-text**, **mxbai-embed-large**, **bge-m3**, **Qwen3-Embedding**.
These are not chat models; Boop uses them only through the embeddings API.

### Autocomplete / fill-in-the-middle

**Qwen2.5-Coder** (all sizes), **StarCoder2**, **CodeGemma**, **Codestral**.
FIM is a different capability from chat or tools; a model can be excellent at
one and poor at another. Boop does not currently use FIM.

---

## Backends

Boop is provider-neutral, so the backend affects operational behaviour rather
than model quality.

| Backend | Tool calling | Strengths | Watch out for |
|:--|:--:|:--|:--|
| **Ollama** | Good | Easiest setup, clean model management, reports capabilities per model | Capability reporting has a real gap — see below |
| **LM Studio** | Good | GUI, easy quant browsing, OpenAI-compatible server | Model must be loaded (or JIT enabled) before use |
| **Lemonade** | Good | AMD/NPU acceleration focus | Boop's adapter here is written against **inferred** endpoints and is unverified |
| **llama.cpp server** | Varies by template | Maximum control, best CPU performance | Tool calling depends on the chat template you supply |
| **vLLM** | Good | Best throughput, real concurrency — a good fit for Boop's parallel agents | Heavier setup, GPU-oriented |

### A verified Ollama quirk worth knowing

Ollama's `/api/tags` **under-reports capabilities**. Verified on 0.31.2:

```
gemma3:12b   /api/tags → ["completion"]           /api/show → ["completion","vision"]
llama3.1:8b  /api/tags → ["completion","tools"]   /api/show → ["completion","tools"]
```

Every gemma3-derived model on the test server showed this. Boop consults
`/api/show` for the authoritative answer because of it — but if you are
reading capabilities yourself, or using another tool that trusts `/api/tags`,
be aware that a missing `vision` there does not mean the model lacks it.

### Checking a model before trusting it

```bash
# What does the server say this model can do?
curl -s http://127.0.0.1:11434/api/show -d '{"model":"qwen2.5:7b"}' | jq .capabilities

# Does tool calling actually work end to end?
curl -s http://127.0.0.1:11434/v1/chat/completions -H 'Content-Type: application/json' -d '{
  "model":"qwen2.5:7b","stream":false,
  "messages":[{"role":"user","content":"What is the weather in Copenhagen? Use the tool."}],
  "tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather",
    "parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]
}' | jq '.choices[0].message.tool_calls'
```

A model that returns `null` there will not drive Boop, whatever else it can do.

---

## Sizing, quantization and VRAM

Rough working-memory guide. Add context on top — a long conversation can add
several GB of KV cache.

| Params | Q4_K_M | Q8 | Practical home for Q4 |
|:--:|:--:|:--:|:--|
| 3B | ~2 GB | ~3.5 GB | Any modern GPU, or CPU |
| 7–8B | ~4.5 GB | ~8 GB | 8 GB card |
| 14B | ~9 GB | ~15 GB | 12–16 GB card |
| 24B | ~14 GB | ~26 GB | 16–24 GB card |
| 32B | ~19 GB | ~35 GB | 24 GB card |
| 30B-A3B (MoE) | ~18 GB | ~33 GB | 24 GB card — but *fast*, only ~3B active |

**Quantization guidance.** `Q4_K_M` is the usual sweet spot. Below Q4,
instruction-following and tool-call formatting degrade noticeably — and
tool-call formatting is precisely what Boop depends on, so aggressive
quantization hurts here more than it would in a chat app. If a model starts
emitting malformed tool calls after you drop a quant level, that is why.

**MoE models** (Qwen3-Coder 30B-A3B, DeepSeek-V2-Lite) need memory for all
parameters but compute only the active ones, so they are much faster than a
dense model of the same footprint. They are a good deal on a 24 GB card or an
Apple unified-memory machine.

---

## Context windows

| Model | Trained context | Realistic working context |
|:--|:--:|:--:|
| Qwen2.5 / Qwen2.5-Coder | 32k (128k with YaRN) | 32k |
| Llama 3.1 / 3.2 | 128k | 32–64k |
| Qwen3-Coder | 256k+ | Large |
| Devstral | 128k | Large |
| Gemma 3 | 128k | 64k+ |

Two cautions. First, **the advertised window is not the served window** —
Ollama and llama.cpp default to a much smaller `num_ctx` (commonly 4096) unless
you raise it, so a 128k model may be silently truncating at 4k. Second,
**quality degrades well before the limit**; most models attend poorly to the
middle of a very long prompt. Boop's context manager evicts and summarises
rather than filling the window, for exactly this reason.

To raise the served context in Ollama, create a Modelfile:

```
FROM qwen2.5-coder:14b
PARAMETER num_ctx 32768
```

---

## Configuring Boop

```yaml
provider: ollama
model: qwen2.5-coder:14b

providers:
  ollama:
    type: ollama
    base_url: http://127.0.0.1:11434
```

Route different jobs to different models — Boop picks per task class:

```yaml
routing:
  default:   { provider: ollama, model: qwen2.5-coder:14b }
  fast:      { provider: ollama, model: qwen2.5-coder:7b }
  reasoning: { provider: ollama, model: qwq:32b }
  vision:    { provider: ollama, model: gemma3:12b }

fallback: [ollama, lmstudio, anthropic]
```

Mixing local and cloud is a reasonable default posture: a local model for the
routine loop, a cloud model for the hard problems.

```yaml
routing:
  default:   { provider: ollama,    model: qwen2.5-coder:14b }
  reasoning: { provider: anthropic, model: claude-sonnet-5 }
```

---

## Troubleshooting

**The model describes tool calls instead of making them.**
Its chat template does not support tools, or it is not tool-tuned. Confirm with
the curl test above. If `tool_calls` is `null`, switch models — no prompting
will fix it.

**It calls a tool once, then stops using tools.**
Usually context truncation: the tool definitions fell out of the served window.
Raise `num_ctx`, or lower `execution.max_tool_iterations`.

**"model does not support tools" (HTTP 400).**
The server refused outright, which is the honest failure. Boop reports this as
an unsupported capability and names alternatives. Pick a model whose
capabilities include `tools`.

**It hallucinates file contents.**
It emitted a pseudo tool call as text and then answered from imagination. Same
cause as the first case. Prefer a coder-tuned model.

**Tool calls got worse after re-quantizing.**
Formatting is one of the first things to degrade below Q4. Go back up a level.

**It works in chat but fails in Boop.**
Boop sends a system prompt, tool schemas and conversation history — far more
than a chat box does. Small models degrade under that load. Try a larger model
or a smaller tool set.

---

## A note on this document

The measured table came from a live server; the capability quirk was verified
against Ollama 0.31.2. The family recommendations reflect the open-model
landscape as of early-to-mid 2026 and **will go stale** — this field moves in
months. Treat the reasoning as durable and the specific model names as
perishable, and re-run the curl test above rather than trusting any list,
including this one.
