// lwdinfo: a probe for a running lightwalletd. Calls GetLightdInfo and GetLatestBlock over
// plaintext gRPC and prints them as one JSON object, so a shell script (the ycash-dd devnet's
// `check`, plan §6.3) can assert the server is up and which node it follows without a gRPC
// client of its own. With -yellowback it also calls every method of the YellowbackStreamer
// service once with a harmless input and reports, per method, "ok" or the gRPC status — the
// devnet's evidence that the service answers (Phase L1 acceptance) and, against a baseline
// server, that every method is UNIMPLEMENTED there.
//
//	go run ./testtools/lwdinfo -server 127.0.0.1:9067 [-timeout 5s] [-yellowback]
//
// Exit 0 with JSON on stdout; exit 1 with the gRPC error on stderr.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/zcash/lightwalletd/walletrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

func main() {
	server := flag.String("server", "127.0.0.1:9067", "lightwalletd address (plaintext gRPC)")
	timeout := flag.Duration("timeout", 5*time.Second, "per-call timeout")
	yellowback := flag.Bool("yellowback", false, "also call every YellowbackStreamer method once")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	conn, err := grpc.DialContext(ctx, *server, grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		fmt.Fprintf(os.Stderr, "lwdinfo: dial %s: %v\n", *server, err)
		os.Exit(1)
	}
	defer conn.Close()
	client := walletrpc.NewCompactTxStreamerClient(conn)

	info, err := client.GetLightdInfo(ctx, &walletrpc.Empty{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "lwdinfo: GetLightdInfo: %v\n", err)
		os.Exit(1)
	}
	out := map[string]interface{}{
		"version":                 info.Version,
		"vendor":                  info.Vendor,
		"taddrSupport":            info.TaddrSupport,
		"chainName":               info.ChainName,
		"saplingActivationHeight": info.SaplingActivationHeight,
		"consensusBranchId":       info.ConsensusBranchId,
		"blockHeight":             info.BlockHeight,
	}
	// The cache may still be filling right after start; report that rather than fail.
	if latest, err := client.GetLatestBlock(ctx, &walletrpc.ChainSpec{}); err == nil {
		out["latestHeight"] = latest.Height
		out["latestHash"] = hex.EncodeToString(latest.Hash)
	} else {
		out["latestError"] = err.Error()
	}
	if *yellowback {
		out["yellowback"] = probeYellowback(conn, *timeout)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		os.Exit(1)
	}
}

// probeYellowback calls each method once. The inputs are harmless: the tip, a txid that does
// not exist (the node answers with its own error, which proves the proxy path), an empty
// selector. A method's value is "ok", "ok (N rows)" for a stream, or "<CODE>: <message>".
func probeYellowback(conn *grpc.ClientConn, timeout time.Duration) map[string]string {
	y := walletrpc.NewYellowbackStreamerClient(conn)
	result := map[string]string{}
	verdict := func(err error) string {
		if err == nil {
			return "ok"
		}
		st, _ := status.FromError(err)
		return st.Code().String() + ": " + st.Message()
	}
	call := func(name string, f func(ctx context.Context) error) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		result[name] = verdict(f(ctx))
	}
	drain := func(name string, recv func() (interface{}, error), err error) error {
		if err != nil {
			return err
		}
		n := 0
		for {
			_, err := recv()
			if err == io.EOF {
				result[name+"#rows"] = fmt.Sprint(n)
				return nil
			}
			if err != nil {
				return err
			}
			n++
		}
	}
	unknownTxid := "0000000000000000000000000000000000000000000000000000000000000001"
	call("GetYellowbackInfo", func(ctx context.Context) error {
		info, err := y.GetYellowbackInfo(ctx, &walletrpc.Empty{})
		if err == nil {
			result["GetYellowbackInfo#rpcversion"] = fmt.Sprint(info.Rpcversion)
			result["GetYellowbackInfo#serverVersion"] = info.ServerVersion
			result["GetYellowbackInfo#height"] = fmt.Sprint(info.Height)
		}
		return err
	})
	call("GetPrice", func(ctx context.Context) error { _, err := y.GetPrice(ctx, &walletrpc.HeightFilter{}); return err })
	call("GetStats", func(ctx context.Context) error { _, err := y.GetStats(ctx, &walletrpc.Empty{}); return err })
	call("GetActivation", func(ctx context.Context) error { _, err := y.GetActivation(ctx, &walletrpc.Empty{}); return err })
	call("GetTxInfo", func(ctx context.Context) error {
		_, err := y.GetTxInfo(ctx, &walletrpc.YedTxid{Txid: unknownTxid})
		return err
	})
	call("ValidateRawTransaction", func(ctx context.Context) error {
		_, err := y.ValidateRawTransaction(ctx, &walletrpc.RawTransaction{Data: []byte{0x04, 0x00, 0x00, 0x80}})
		return err
	})
	call("DecodePayload", func(ctx context.Context) error {
		_, err := y.DecodePayload(ctx, &walletrpc.YedHex{Hex: "59420301"})
		return err
	})
	call("GetVault", func(ctx context.Context) error {
		_, err := y.GetVault(ctx, &walletrpc.YedTxid{Txid: unknownTxid})
		return err
	})
	call("ListVaults", func(ctx context.Context) error {
		s, err := y.ListVaults(ctx, &walletrpc.YedVaultFilter{})
		return drain("ListVaults", func() (interface{}, error) { return s.Recv() }, err)
	})
	call("ListClaimable", func(ctx context.Context) error {
		s, err := y.ListClaimable(ctx, &walletrpc.Empty{})
		return drain("ListClaimable", func() (interface{}, error) { return s.Recv() }, err)
	})
	call("GetNotice", func(ctx context.Context) error {
		_, err := y.GetNotice(ctx, &walletrpc.YedTxid{Txid: unknownTxid})
		return err
	})
	call("EstimateCollateral", func(ctx context.Context) error {
		est, err := y.EstimateCollateral(ctx, &walletrpc.YedMintQuery{Cents: 10000, LockBlocks: 48})
		if err == nil {
			result["EstimateCollateral#requiredZat"] = fmt.Sprint(est.RequiredZat)
			result["EstimateCollateral#refHeight"] = fmt.Sprint(est.RefHeight)
		}
		return err
	})
	call("EstimateFee", func(ctx context.Context) error {
		_, err := y.EstimateFee(ctx, &walletrpc.YedFeeQuery{CollateralZat: 100000000})
		return err
	})
	call("GetFeePayee", func(ctx context.Context) error {
		_, err := y.GetFeePayee(ctx, &walletrpc.YedPayeeQuery{RefHeight: 1, CollateralZat: 100000000})
		return err
	})
	call("BuildBundle", func(ctx context.Context) error {
		_, err := y.BuildBundle(ctx, &walletrpc.YedBundleQuery{RefHeight: 1})
		return err
	})
	call("GetSelection", func(ctx context.Context) error {
		_, err := y.GetSelection(ctx, &walletrpc.YedBundleQuery{RefHeight: 1})
		return err
	})
	call("GetAttestations", func(ctx context.Context) error {
		s, err := y.GetAttestations(ctx, &walletrpc.Empty{})
		return drain("GetAttestations", func() (interface{}, error) { return s.Recv() }, err)
	})
	call("ListAttestors", func(ctx context.Context) error {
		s, err := y.ListAttestors(ctx, &walletrpc.HeightFilter{})
		return drain("ListAttestors", func() (interface{}, error) { return s.Recv() }, err)
	})
	return result
}
