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

package digest

import (
	"reflect"
	"testing"
)

func TestGeneric(t *testing.T) {
	in := []byte("FAILURE: anything at all\ne: looks/like.kt:1:1 an error\n--- FAIL: TestX (0s)\n")
	if got := Generic(in); !reflect.DeepEqual(got, Findings{}) {
		t.Errorf("Generic must extract nothing, got %+v", got)
	}
}
