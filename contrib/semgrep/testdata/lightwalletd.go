// Test cases for contrib/semgrep/rules/lightwalletd.yaml
// (`semgrep --test --config contrib/semgrep/rules contrib/semgrep/testdata`). Never built: Go skips
// testdata directories, and .semgrepignore keeps contrib/semgrep out of the scan itself.
package frontend

import (
	"context"
	"encoding/hex"
	"encoding/json"

	"github.com/sirupsen/logrus"
	"github.com/zcash/lightwalletd/common"
	"github.com/zcash/lightwalletd/walletrpc"
)

func rpcCalls(ctx context.Context, method string, params []json.RawMessage) {
	// ok: lightwalletd.rpc.node-wallet-or-write-method
	common.RawRequest("getblock", params)
	// ok: lightwalletd.rpc.node-wallet-or-write-method
	common.RawRequest("sendrawtransaction", params)
	// ok: lightwalletd.rpc.node-wallet-or-write-method
	common.CallYed(ctx, "yed_getvault", params...)
	// ruleid: lightwalletd.rpc.node-wallet-or-write-method
	common.RawRequest("yed_mint", params)
	// ruleid: lightwalletd.rpc.node-wallet-or-write-method
	common.CallYed(ctx, "vault_lock", params...)
	// ruleid: lightwalletd.rpc.node-wallet-or-write-method
	common.RawRequest("z_sendmany", params)
	// ruleid: lightwalletd.rpc.node-wallet-or-write-method
	common.RawRequest("dumpprivkey", params)

	// ruleid: lightwalletd.rpc.dynamic-method-outside-gate
	common.RawRequest(method, params)
	// ok: lightwalletd.rpc.dynamic-method-outside-gate
	common.RawRequest("getinfo", params)
}

type YellowbackStreamer struct{}

func (y *YellowbackStreamer) limited(ctx context.Context, cost int) error { return nil }

// ruleid: lightwalletd.grpc.yellowback-handler-without-limiter
func (y *YellowbackStreamer) GetUnlimited(ctx context.Context, in *walletrpc.Empty) (*walletrpc.Empty, error) {
	return &walletrpc.Empty{}, nil
}

// ok: lightwalletd.grpc.yellowback-handler-without-limiter
func (y *YellowbackStreamer) GetLimited(ctx context.Context, in *walletrpc.Empty) (*walletrpc.Empty, error) {
	if err := y.limited(ctx, 1); err != nil {
		return nil, err
	}
	return &walletrpc.Empty{}, nil
}

// ruleid: lightwalletd.grpc.yellowback-handler-without-limiter
func (y *YellowbackStreamer) StreamUnlimited(in *walletrpc.Empty, stream walletrpc.CompactTxStreamer_GetMempoolTxServer) error {
	return nil
}

// ok: lightwalletd.grpc.yellowback-handler-without-limiter
func (y *YellowbackStreamer) StreamLimited(in *walletrpc.Empty, stream walletrpc.CompactTxStreamer_GetMempoolTxServer) error {
	if err := y.limited(stream.Context(), 4); err != nil {
		return err
	}
	return nil
}

// ok: lightwalletd.grpc.yellowback-handler-without-limiter
func (y *YellowbackStreamer) helper(ctx context.Context, in *walletrpc.Empty) (*walletrpc.Empty, error) {
	return nil, nil
}

type lwdStreamer struct{}

func (s *lwdStreamer) Unbounded(span *walletrpc.BlockRange, resp walletrpc.CompactTxStreamer_GetBlockRangeServer) error {
	// ruleid: lightwalletd.grpc.request-range-unbounded
	use(span.Start.Height, span.End.Height)
	return nil
}

func (s *lwdStreamer) Bounded(span *walletrpc.BlockRange, resp walletrpc.CompactTxStreamer_GetBlockRangeServer) error {
	if span.End.Height-span.Start.Height > 10000 {
		return nil
	}
	// ok: lightwalletd.grpc.request-range-unbounded
	use(span.Start.Height, span.End.Height)
	return nil
}

func use(a, b uint64) {}

func logging(rawtx *walletrpc.RawTransaction, opts common.Options) {
	// ruleid: lightwalletd.log.secret-or-raw-tx
	common.Log.Info("tx ", hex.EncodeToString(rawtx.Data))
	// ruleid: lightwalletd.log.secret-or-raw-tx
	common.Log.Debugf("pass %s", opts.RPCPassword)
	// ok: lightwalletd.log.secret-or-raw-tx
	common.Log.Info("tx bytes ", len(rawtx.Data))
}

func optsLogging() {
	opts := &common.Options{RPCUser: "u", RPCPassword: "p"}
	// ruleid: lightwalletd.log.secret-or-raw-tx
	common.Log.Debugf("Options: %#v\n", opts)
}

func fieldLogging(rawtx *walletrpc.RawTransaction) {
	// ruleid: lightwalletd.log.secret-or-raw-tx
	common.Log.WithFields(logrus.Fields{"txid": "x", "raw": hex.EncodeToString(rawtx.Data)}).Info("sent")
	// ok: lightwalletd.log.secret-or-raw-tx
	common.Log.WithFields(logrus.Fields{"len": len(rawtx.Data)}).Info("sent")
}
