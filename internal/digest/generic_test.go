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
