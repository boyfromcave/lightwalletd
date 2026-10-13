// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

// The devnet suite's wallet-notification barrier, untagged so the push tier tests it offline.
//
// ycashd (both node lines, as upstream zcashd) tells its wallet about connected blocks and new
// mempool transactions on a separate thread that wakes once a second (ThreadNotifyWallets,
// src/validationinterface.cpp). Until it has run, a wallet transaction a new block just confirmed
// is in neither the chain the wallet knows nor the mempool, so CMerkleTx::GetDepthInMainChain
// answers -1 and CWallet::IsSpent counts its inputs unspent (src/wallet/wallet.cpp): a
// sendtoaddress in that window can choose one of them again, and AcceptToMemoryPool refuses it
// with "inputs already spent" (RPC -4 "The transaction was rejected!"). On regtest the node
// reports the barrier as getblockchaininfo.fullyNotified and getmempoolinfo.fullyNotified; the
// Python framework's sync_blocks/sync_mempools wait for both, and so must the Go devnet suite
// before node 0's wallet acts on a block the pool just mined (nightly 38340276753:
// TestDevnetPQTaddrThroughServer's first sendtoaddress, ~40 ms after the previous test's block).
package frontend

import (
	"encoding/json"
	"fmt"
	"time"
)

// waitWalletNotified polls call (node 0's RPC) until both getblockchaininfo.fullyNotified and
// getmempoolinfo.fullyNotified are true, or timeout passes. A node that does not report the
// field (not regtest) is an error: the suite cannot know its wallet is current.
func waitWalletNotified(call func(method string, params []json.RawMessage) (json.RawMessage, error), timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		notified := true
		for _, method := range []string{"getblockchaininfo", "getmempoolinfo"} {
			raw, err := call(method, nil)
			if err != nil {
				return fmt.Errorf("%s: %v", method, err)
			}
			var info struct {
				FullyNotified *bool `json:"fullyNotified"`
			}
			if err := json.Unmarshal(raw, &info); err != nil {
				return fmt.Errorf("%s: %v", method, err)
			}
			if info.FullyNotified == nil {
				return fmt.Errorf("%s reports no fullyNotified (a regtest-only field): is node 0 on regtest?", method)
			}
			if !*info.FullyNotified {
				notified = false
			}
		}
		if notified {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("node 0's wallet was not notified of the tip and mempool within %v", timeout)
		}
		time.Sleep(poll)
	}
}
