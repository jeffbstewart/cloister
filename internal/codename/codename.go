// Copyright 2026 Jeffrey B. Stewart
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package codename mints short two-word labels — "brisk-otter",
// "amber-kestrel" — for the common case where a name is needed but
// nobody has an opinion about it.  Its first consumer is start_work: a
// line of work usually deserves a handle, not a title, and making the
// operator (or the model) coin one is friction that produces
// agent/fix-thing, agent/fix-thing-2, agent/fix-thing-final.
//
// The words are deliberately BLAND.  A codename should be memorable and
// pronounceable without describing, judging, or predicting anything: it
// is a handle for conversation ("what happened on brisk-otter?"), and a
// name that claims something ("agent/quick-fix") ages badly the moment
// the work turns out otherwise.
//
// Every word is lowercase ASCII, 3–10 letters, so a pair joined by a
// hyphen is a legal git branch name and a legal path segment with no
// escaping anywhere.
package codename

import "math/rand/v2"

// adjectives and animals are the two halves of the vocabulary: 50 each,
// so 2500 pairs.  Animals throughout — they are concrete, easy to say
// aloud, and carry no connotation about the work.  Neither list repeats
// a word from the other, so a pair never stutters ("swift-swift").
var adjectives = []string{
	"amber", "azure", "bold", "brave", "brisk",
	"calm", "clear", "crisp", "dusky", "eager",
	"early", "fair", "fleet", "gentle", "glad",
	"grand", "hardy", "hollow", "humble", "ivory",
	"jolly", "keen", "lively", "lofty", "loyal",
	"lucid", "mellow", "merry", "mild", "misty",
	"noble", "olive", "placid", "prime", "quick",
	"quiet", "rapid", "ready", "russet", "silent",
	"silver", "sleek", "slender", "smooth", "snowy",
	"solid", "steady", "stout", "sunny", "swift",
}

var animals = []string{
	"otter", "badger", "heron", "marten", "falcon",
	"ibex", "lynx", "osprey", "raven", "sparrow",
	"beaver", "bison", "crane", "dormouse", "egret",
	"ferret", "finch", "gannet", "gecko", "gibbon",
	"hare", "hedgehog", "ibis", "jackdaw", "kestrel",
	"lapwing", "lemur", "magpie", "mole", "narwhal",
	"newt", "nuthatch", "ocelot", "pelican", "pika",
	"plover", "puffin", "quail", "rabbit", "salmon",
	"seal", "shrew", "skylark", "stoat", "tapir",
	"tern", "vole", "walrus", "weasel", "wren",
}

// Combinations is how many distinct codenames exist.  Collisions are the
// caller's problem to notice (a branch that already exists), not this
// package's to prevent: it holds no state and mints no sequence.
const Combinations = 50 * 50

// New mints a codename, e.g. "brisk-otter".
func New() string { return Pick(rand.IntN) }

// Pick mints a codename using the caller's chooser — intn(n) must return
// a value in [0,n).  Tests pin it; New passes the global source.
func Pick(intn func(n int) int) string {
	return adjectives[intn(len(adjectives))] + "-" + animals[intn(len(animals))]
}
