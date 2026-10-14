package main

import "embed"

// bundledVoices holds the reference voice shipped inside the program: a short clip
// (assets/voices/default.wav, or jarvis.wav) plus its transcript (default.txt /
// jarvis.txt). The audio files are not stored in git, so a build from a fresh clone
// has only the README and the assistant falls back to a described voice.
//
//go:embed assets/voices
var bundledVoices embed.FS
