package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

func TestEachJSONOneDocument(t *testing.T) {
	var out, errb bytes.Buffer
	c := &cli{out: &out, err: &errb, asJSON: true, cur: &command{}}
	err := c.each([]string{"a", "b", "c"}, false, func(s string) error {
		if s == "b" {
			return errors.New("boom")
		}
		_, _ = io.WriteString(c.out, `{"id":"`+s+`"}`)
		return nil
	})
	if err == nil {
		t.Fatal("want exit 1")
	}
	var v []map[string]string
	if e := json.Unmarshal(out.Bytes(), &v); e != nil || len(v) != 2 || v[1]["id"] != "c" {
		t.Errorf("out=%q err=%v", out.String(), e)
	}
}
