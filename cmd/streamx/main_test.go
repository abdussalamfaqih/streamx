package main

import (
	"flag"
	"io"
	"reflect"
	"testing"
)

func TestParseArgs(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	env := fs.String("env", "", "")
	v := fs.Bool("v", false, "")
	pos, err := parseArgs(fs, []string{"a", "--env", "local", "b", "-v"})
	if err != nil || !reflect.DeepEqual(pos, []string{"a", "b"}) || *env != "local" || !*v {
		t.Errorf("pos=%v env=%q v=%v err=%v", pos, *env, *v, err)
	}
	if _, err := parseArgs(fs, []string{"--nope"}); err == nil {
		t.Error("unknown flag accepted")
	}
}
