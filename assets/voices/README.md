# Reference voice

OmniVoice clones the assistant's voice from a short reference recording.

Put two files here **before building** and they are embedded in the program:

- `default.wav` (or `jarvis.wav`): 5-15 seconds of clean speech, 16-bit mono WAV
- `default.txt` (or `jarvis.txt`): the exact words spoken in the clip

Convert other formats first, e.g. `ffmpeg -i clip.mp3 -ac 1 -ar 24000 -c:a pcm_s16le default.wav`.

`jarvis.wav` is committed so every build carries the same voice. Only commit recordings you have the right to
share; the `.mp3` source is gitignored. Without a clip the assistant uses a described voice
(`male, middle-aged, british accent, moderate pitch`).

To override the voice on an installed copy, place `default.wav` and `default.txt` in
`%LOCALAPPDATA%\SovereignAI\voices\`.

OmniVoice's weights are CC-BY-NC 4.0: personal and non-commercial use only.
