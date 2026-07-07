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

package audit

// The librarian's audit vocabulary.  Reads are audited on DENIAL only
// (docs/librarian.md): successful reads would change the ledger's
// character, and the interesting event is refusal.

// DecisionReadDenied: a content operation named a shielded path.
const DecisionReadDenied Decision = "denied_read"

// ReadDetail is the librarian's denial record: the denied path or paths
// (plural — a batch op may deny several at once) and nothing else.  The
// tool is in the Header; arguments are deliberately not recorded.
type ReadDetail struct {
	Paths []string `json:"paths"`
}
