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

package sink

import (
	"testing"
	"time"
)

func TestAllowBurstThenDeny(t *testing.T) {
	b := newBucket(1, 2)
	if !b.allow(1) || !b.allow(1) {
		t.Fatal("burst capacity not honored")
	}
	if b.allow(1) {
		t.Error("allow succeeded past the burst with no refill time")
	}
}

func TestAllowRefills(t *testing.T) {
	b := newBucket(100, 1)
	if !b.allow(1) {
		t.Fatal("first token missing")
	}
	if b.allow(1) {
		t.Fatal("bucket should be empty")
	}
	time.Sleep(50 * time.Millisecond) // ~5 tokens at 100/s, capped at burst 1
	if !b.allow(1) {
		t.Error("bucket did not refill over time")
	}
}

func TestTakePacesLargeRequests(t *testing.T) {
	b := newBucket(10_000, 1_000)
	start := time.Now()
	b.take(3_000) // 1000 burst + 2000 paced at 10k/s ≈ 200ms
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Errorf("take returned in %s; pacing not applied", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("take took %s; pacing far too slow", elapsed)
	}
}
