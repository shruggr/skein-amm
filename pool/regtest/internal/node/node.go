// Package node is a minimal JSON-RPC client for a bitcoin-sv regtest node.
//
// Copied (trimmed to what the pool suite uses) from Rúnar's integration
// helpers, runar/integration/go/helpers/rpc.go (RPCCall, Mine,
// SendToAddress, SendRawTransaction, GetRawTransaction, GetTxOut,
// EnsureRegtest, GetBlockCount). The helpers package itself is importable
// (module runar-integration, via a replace), but it compiles compiler.go
// against Rúnar's codegen/frontend/ir packages, which ties this module to one
// Rúnar compiler API and pulls in gnark; this suite also runs against the
// pinned Rúnar, so it carries its own copy. SV Node only (no Teranode path).
package node

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultRPCURL is the `skein-amm` instance of Rúnar's integration/regtest.sh:
// `regtest.sh start skein-amm` puts its RPC port at 19000 + cksum("skein-amm")
// % 800 = 19246. Override with RPC_URL (e.g. http://localhost:18332 for the
// default instance).
const DefaultRPCURL = "http://localhost:19246"

var rpcID uint64

var client = &http.Client{Timeout: 10 * time.Minute}

// URL is the node's RPC endpoint.
func URL() string {
	if u := os.Getenv("RPC_URL"); u != "" {
		return u
	}
	return DefaultRPCURL
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// RPCError is the node's own refusal ("RPC error <code>: <message>"), as
// opposed to a transport failure.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("RPC error %d: %s", e.Code, e.Message) }

// Call makes a JSON-RPC 1.0 call.
func Call(method string, params ...interface{}) (json.RawMessage, error) {
	if params == nil {
		params = []interface{}{}
	}
	body, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "1.0", "id": atomic.AddUint64(&rpcID, 1), "method": method, "params": params,
	})
	req, err := http.NewRequest("POST", URL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(env("RPC_USER", "bitcoin"), env("RPC_PASS", "bitcoin"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("RPC connection failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("RPC response parse error: %w (body: %s)", err, raw)
	}
	if r.Error != nil {
		return nil, r.Error
	}
	return r.Result, nil
}

// EnsureRegtest refuses to run against anything but regtest.
func EnsureRegtest() error {
	res, err := Call("getblockchaininfo")
	if err != nil {
		return err
	}
	var info struct {
		Chain  string `json:"chain"`
		Blocks int    `json:"blocks"`
	}
	if err := json.Unmarshal(res, &info); err != nil {
		return err
	}
	if info.Chain != "regtest" {
		return fmt.Errorf("SAFETY: node is on %q, not regtest", info.Chain)
	}
	return nil
}

// BlockCount is the current height.
func BlockCount() (int, error) {
	res, err := Call("getblockcount")
	if err != nil {
		return 0, err
	}
	var n int
	err = json.Unmarshal(res, &n)
	return n, err
}

// mineMu serializes mining: two generatetoaddress calls racing on one tip
// make competing blocks, and a transaction mined only in the losing one reads
// back from txindex with 0 confirmations although the winning chain has it.
var mineMu sync.Mutex

// Mine generates n blocks to a node-wallet address.
func Mine(n int) error {
	mineMu.Lock()
	defer mineMu.Unlock()
	res, err := Call("getnewaddress")
	if err != nil {
		return err
	}
	var addr string
	if err := json.Unmarshal(res, &addr); err != nil {
		return err
	}
	_, err = Call("generatetoaddress", n, addr)
	return err
}

// SendToAddress pays sats from the node wallet to addr and returns the txid.
func SendToAddress(addr string, sats int64) (string, error) {
	// The RPC takes BSV as a JSON number; json.Number keeps it exact.
	amount := json.Number(fmt.Sprintf("%d.%08d", sats/100_000_000, sats%100_000_000))
	res, err := Call("sendtoaddress", addr, amount)
	if err != nil {
		return "", err
	}
	var txid string
	err = json.Unmarshal(res, &txid)
	return txid, err
}

// SendRawTransaction broadcasts a raw transaction.
func SendRawTransaction(txHex string) (string, error) {
	res, err := Call("sendrawtransaction", txHex)
	if err != nil {
		return "", err
	}
	var txid string
	err = json.Unmarshal(res, &txid)
	return txid, err
}

// RawTransactionHex is a transaction's serialization (needs txindex=1 once
// mined, which regtest.sh sets).
func RawTransactionHex(txid string) (string, error) {
	res, err := Call("getrawtransaction", txid, 0)
	if err != nil {
		return "", err
	}
	var h string
	err = json.Unmarshal(res, &h)
	return h, err
}

// Confirmations is getrawtransaction's confirmation count (0 in the mempool).
func Confirmations(txid string) (int, string, error) {
	res, err := Call("getrawtransaction", txid, 1)
	if err != nil {
		return 0, "", err
	}
	var v struct {
		Confirmations int    `json:"confirmations"`
		BlockHash     string `json:"blockhash"`
	}
	err = json.Unmarshal(res, &v)
	return v.Confirmations, v.BlockHash, err
}

// TxOut is gettxout (mempool included): nil when the output is spent, in a
// block or in the mempool, or never existed.
func TxOut(txid string, vout int) (map[string]interface{}, error) {
	res, err := Call("gettxout", txid, vout, true)
	if err != nil {
		return nil, err
	}
	if string(res) == "null" {
		return nil, nil
	}
	var out map[string]interface{}
	err = json.Unmarshal(res, &out)
	return out, err
}
