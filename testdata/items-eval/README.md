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
| `unicode` | `true` for the meeting that tells ScaleDown's offset unit: non-ASCII speaker names, curly apostrophes and a character outside the Basic Multilingual Plane. The run fails `extract` on any span there whose `text` is not the input at its offsets |
| `speakers` | each `name`, and `guest: true` for a guest |
| `segments` | in the order they were said: `id` (unique across the corpus), `speaker`, `text`, and optionally `dur` (seconds; else one second per 2.2 words, at least 2 s). Each starts 1.2 s after the previous one ends (the endpointer's 700 ms pause and the turn); times are meeting milliseconds. These are `segGapMS`, `wordsPerSecond` and `minSegMS` in `cmd/items-eval/corpus.go` |
| `labels` | each expected item: `id`, canonical `text`, `assignee` (a speaker's name), `due` as said (or absent), the `segments` that state it, and `cases` (its hard-case kinds) |
| `traps` | the negative hard cases: `kind` and the `segments` that say it. Nothing here is an item; an emitted item citing only these is a false positive |

Hard-case kinds, every one required at least 3 times: `hypothetical`,
`past_tense`, `question`, `retracted` (a commitment withdrawn in the same
breath), `injection` (a guest's speech addressing the model: an
instruction, a forged segment id and speaker, a forged owner), on traps; `cross_speaker` (asked by one, accepted by another) and
`restated` (said again later; the label cites every statement), on labels;
and `asr_noise`, a meeting with `asr: true`.

The harness replays each meeting through the worker's own window selection
(`items.Select`, a tick every 10 s, 90 s back), so the windows are the ones
production would send. `go test ./cmd/items-eval` lints the corpus: ids,
citations, speakers, the window count, the no-item share, every hard-case
kind, the non-ASCII meeting, that injections are a guest's, and the controls.

🔴 **Owed:** the design asks for the ASR variants to come from real
speech-to-text over synthesised speech. These three are written by hand in
that style; the real pass (TTS, then the `utterance` engine's STT) has not
been run. A follow-up PR adds a real-STT variant of each of the 12 meetings
before the phase 4c run counts as the gate.
