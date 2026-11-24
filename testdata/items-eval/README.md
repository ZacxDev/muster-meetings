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

**The speech-to-text corpus.** Beside each meeting `<name>.yaml` is
`<name>.stt.yaml`: the same meeting spoken by Piper (a distinct voice per
speaker, British and American, `cmd/items-corpus-stt/voices.yaml`) and
transcribed by the plugin's own STT engine (`nemotron`, as live calls are
stored: one stream per speaker, 80 ms frames in real time, silence
included, a flush per utterance, late closing marks amended onto the line
they end). The ids, speakers, labels and traps are the source's; only each
segment's `text` is the STT's, and its times are the source's (so the
windows are the same). Its `stt` block records the engine (and the lines
per engine), each speaker's voice, the tool's version and the word error
rate against the source text (summed per segment; numbers are not spelled
out, so "ten" and "10" differ). A segment the engine heard nothing in is
left out of the timeline, as production stores no line for it.

**The gate is both corpora.** `cmd/items-eval` replays every model on
both and reports each, and a model passes only on the worst of the two:
every metric the worse corpus's, so it must pass on both; the choice reads
the same worst-of-both numbers. The controls must pass on each. Its lint requires every source to have its
variant, the variant to match its source in everything but the text, and
the recorded word error rate to be what the text gives. The three
`asr: true` meetings are written by hand in speech-to-text style; their
variants are STT over that.

Remake the variants (by hand, never in CI: it needs the network, the Piper
voices and the STT servers' GPU) with `cmd/items-corpus-stt`; its doc
comment has the environment it reads.
