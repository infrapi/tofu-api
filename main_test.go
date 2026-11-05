package main

import (
	"bytes"
	"errors"
	"flag"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The command line: help and version on stdout, a line tofu-api does not
// understand a usage error (exit 2), before any configuration is read.
func TestRunCommandLine(t *testing.T) {
	for _, c := range []struct {
		args       []string
		stdout     string
		stderr     string
		usage      bool
		shown      bool
		help       bool
		errContain string
	}{
		{args: nil, stderr: "Commands:", shown: true},
		{args: []string{"-h"}, stdout: "Usage:"},
		{args: []string{"help"}, stdout: "tofu-api version"},
		{args: []string{"version"}, stdout: "tofu-api "},
		{args: []string{"nope"}, usage: true, errContain: `unknown command "nope"`},
		{args: []string{"gen"}, usage: true, errContain: "gen: -config is required, run 'tofu-api gen -h'"},
		{args: []string{"serve", "-config", "x.yaml", "extra"}, usage: true, errContain: `serve: unexpected argument "extra"`},
		{args: []string{"gen", "-h"}, stderr: "-o file", help: true},
		{args: []string{"serve", "-h"}, stderr: "-config file", help: true},
		{args: []string{"serve", "-config", "x.yaml", "-o", "y"}, stderr: "flag provided but not defined: -o", shown: true},
	} {
		var stdout, stderr bytes.Buffer
		err := run(c.args, &stdout, &stderr)
		switch {
		case c.usage:
			var u usageError
			require.ErrorAs(t, err, &u, c.args)
			assert.Contains(t, err.Error(), c.errContain, c.args)
		case c.shown:
			assert.ErrorIs(t, err, errShown, c.args)
		case c.help:
			assert.True(t, errors.Is(err, flag.ErrHelp), c.args)
		default:
			require.NoError(t, err, c.args)
		}
		assert.Contains(t, stdout.String(), c.stdout, c.args)
		assert.Contains(t, stderr.String(), c.stderr, c.args)
	}
}
