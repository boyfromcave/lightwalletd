// Copyright (c) 2019-2020 The Zcash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .
package cmd

import (
	"strings"
	"testing"

	"github.com/zcash/lightwalletd/common"
)

func TestFileExists(t *testing.T) {
	if fileExists("nonexistent-file") {
		t.Fatal("fileExists unexpected success")
	}
	// If the path exists but is a directory, should return false
	if fileExists(".") {
		t.Fatal("fileExists unexpected success")
	}
	// The following file should exist, it's what's being tested
	if !fileExists("root.go") {
		t.Fatal("fileExists failed")
	}
}

func TestOptionsForLogOmitsRPCPassword(t *testing.T) {
	got := optionsForLog(&common.Options{RPCUser: "alice", RPCPassword: "s3cret-pw", RPCPort: "8232"})
	if strings.Contains(got, "s3cret-pw") || strings.Contains(got, "RPCPassword") {
		t.Fatalf("optionsForLog leaks the RPC password: %s", got)
	}
	if !strings.Contains(got, `RPCUser:"alice"`) || !strings.Contains(got, `RPCPort:"8232"`) {
		t.Fatalf("optionsForLog lost the other fields: %s", got)
	}
}
