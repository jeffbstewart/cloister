package main

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

func main() {
	y := "actions:\n  build:\n    run: [\"make\"]\n  build:\n    run: [\"make2\"]\n"
	dec := yaml.NewDecoder(bytes.NewReader([]byte(y)))
	dec.KnownFields(true)
	var m map[string]any
	err := dec.Decode(&m)
	fmt.Printf("err=%v\nm=%v\n", err, m)
}
