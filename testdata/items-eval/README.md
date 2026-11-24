# The action-item evaluation corpus

Twelve meetings written for `cmd/items-eval` (design: "Action-item
evaluation"). **Synthetic:** no real meeting, no real person, no real
company. Every name is a stock first name; every product and team is
invented.

One YAML file per meeting:

| field | what |
|---|---|
| `id`, `kind`, `title` | `kind` is `standup`, `planning`, `customer` or `one_to_one` |
| `asr` | `true` for a meeting written in speech-to-text style (lower case, no punctuation, misheard words) |
| `speakers` | each `name`, and `guest: true` for a guest |
| `segments` | in the order they were said: `id` (unique across the corpus), `speaker`, `text`, and optionally `dur` (seconds; else one second per 2.6 words, at least 1.5 s). Each starts 0.6 s after the previous one ends; times are meeting milliseconds |
| `labels` | each expected item: `id`, canonical `text`, `assignee` (a speaker's name), `due` as said (or absent), the `segments` that state it, and `cases` (its hard-case kinds) |
| `traps` | the negative hard cases: `kind` and the `segments` that say it. Nothing here is an item; an emitted item citing only these is a false positive |

Hard-case kinds: `hypothetical`, `past_tense`, `question`, `retracted` (a
commitment withdrawn in the same breath), `injection` (speech that addresses
the model), on traps; `cross_speaker` (asked by one, accepted by another) and
`restated` (said again later; the label cites every statement), on labels;
and `asr_noise`, a meeting with `asr: true`.

The harness replays each meeting through the worker's own window selection
(`items.Select`, a tick every 10 s, 90 s back), so the windows are the ones
production would send. `go test ./cmd/items-eval` lints the corpus: ids,
citations, speakers, the window count, the no-item share, every hard-case
kind, and the controls.

🔴 **Owed:** the design asks for the ASR variants to come from real
speech-to-text over synthesised speech. These three are written by hand in
that style; the real pass (TTS, then the `utterance` engine's STT) has not
been run.
