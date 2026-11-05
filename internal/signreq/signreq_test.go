package signreq

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/identity"
	"defios.com/internal/key"
	"defios.com/internal/network"
	"defios.com/internal/session"
	coslv1 "defios.com/proto/cosl/v1"
)

const b58 = "11111111111111111111111111111111"

var (
	// nets are a Node's local Networks; a request names their chains.
	nets = []network.Network{
		{Alias: "eth", Type: "ethereum", ChainID: 1},
		{Alias: "btc", Type: "bitcoin", BitcoinNetwork: "mainnet"},
		{Alias: "sol", Type: "solana", Cluster: "devnet"},
	}
	ecdsaKey = key.Key{Curve: "ecdsa", PublicKey: mustHex(btcPubkey)} // the key btcTx() spends from
	eddsaKey = key.Key{Curve: "eddsa", PublicKey: make([]byte, 32)}   // the address b58 spells
)

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func ethTx() *coslv1.SignTransaction {
	return &coslv1.SignTransaction{Intent: &coslv1.SignTransaction_Ethereum{Ethereum: &coslv1.EthereumIntent{
		To: make([]byte, 20), Value: "1000", GasLimit: 21000, MaxFeePerGas: "30000000000", MaxPriorityFeePerGas: "1", ChainId: 1,
	}}}
}

func btcTx() *coslv1.SignTransaction {
	return &coslv1.SignTransaction{Intent: &coslv1.SignTransaction_Bitcoin{Bitcoin: &coslv1.BitcoinIntent{
		Inputs:  []*coslv1.BitcoinInput{{Txid: strings.Repeat("ab", 32), AmountSats: 5000, ScriptPubkey: mustHex(btcSPK)}},
		Outputs: []*coslv1.BitcoinOutput{{Address: "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", AmountSats: 4000}},
		Network: "mainnet",
	}}}
}

func solTx() *coslv1.SignTransaction {
	return &coslv1.SignTransaction{Intent: &coslv1.SignTransaction_Solana{Solana: &coslv1.SolanaIntent{
		FeePayer: b58, RecentBlockhash: b58,
		Instructions: []*coslv1.SolanaInstruction{{ProgramId: b58}},
		Cluster:      "devnet",
	}}}
}

func req(txs ...*coslv1.SignTransaction) *coslv1.SignRequest {
	return &coslv1.SignRequest{Transactions: txs, Reason: "pay the invoice", DeadlineSeconds: 300}
}

func TestValidate(t *testing.T) {
	good := []struct {
		name string
		r    *coslv1.SignRequest
		k    key.Key
	}{
		{"eth", req(ethTx()), ecdsaKey},
		{"eth+btc mixed same curve", req(ethTx(), btcTx()), ecdsaKey},
		{"sol", req(solTx()), eddsaKey},
	}
	for _, c := range good {
		if err := Validate(c.r, c.k); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}

	noReason := req(ethTx())
	noReason.Reason = " "
	noDeadline := req(ethTx())
	noDeadline.DeadlineSeconds = 0
	badNonUTF := req(ethTx())
	badNonUTF.Reason = "\xff"
	noChainID := ethTx()
	noChainID.GetEthereum().ChainId = 0
	noBtcNetwork := btcTx()
	noBtcNetwork.GetBitcoin().Network = ""
	badBtcNetwork := btcTx()
	badBtcNetwork.GetBitcoin().Network = "mainnet2"
	noCluster := solTx()
	noCluster.GetSolana().Cluster = ""
	badGenesis := solTx()
	badGenesis.GetSolana().GenesisHash = "not base58!"
	noIntent := &coslv1.SignTransaction{}
	badTo := ethTx()
	badTo.GetEthereum().To = []byte{1}
	noGas := ethTx()
	noGas.GetEthereum().GasLimit = 0
	badValue := ethTx()
	badValue.GetEthereum().Value = "1e3"
	noInputs := btcTx()
	noInputs.GetBitcoin().Inputs = nil
	noBlockhash := solTx()
	noBlockhash.GetSolana().RecentBlockhash = ""
	tipOverMax := ethTx()
	tipOverMax.GetEthereum().MaxPriorityFeePerGas = "31000000000"
	hugeValue := ethTx()
	hugeValue.GetEthereum().Value = strings.Repeat("9", 100)
	wrapBtc := btcTx()
	wrapBtc.GetBitcoin().Inputs = []*coslv1.BitcoinInput{
		{Txid: strings.Repeat("ab", 32), AmountSats: 1 << 63, ScriptPubkey: mustHex(btcSPK)},
		{Txid: strings.Repeat("ab", 32), AmountSats: 1 << 63, ScriptPubkey: mustHex(btcSPK)},
	}
	foreignInput := btcTx() // an input the Key does not own
	foreignInput.GetBitcoin().Inputs[0].ScriptPubkey = mustHex("0014" + strings.Repeat("11", 20))
	taprootInput := btcTx() // P2TR is not a script type a Key signs for
	taprootInput.GetBitcoin().Inputs[0].ScriptPubkey = mustHex("5120" + strings.Repeat("11", 32))
	badAddress := btcTx()
	badAddress.GetBitcoin().Outputs[0].Address = "bc1qxyz"
	testnetAddress := btcTx() // a testnet address on the mainnet Network
	testnetAddress.GetBitcoin().Outputs[0].Address = "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"
	badTxid := btcTx()
	badTxid.GetBitcoin().Inputs[0].Txid = "zz"
	badPayer := solTx()
	badPayer.GetSolana().FeePayer = "xyz"
	longDeadline := req(ethTx())
	longDeadline.DeadlineSeconds = 1 << 40

	bad := map[string]struct {
		r *coslv1.SignRequest
		k key.Key
	}{
		"no transactions":   {req(), ecdsaKey},
		"blank reason":      {noReason, ecdsaKey},
		"non-utf8":          {badNonUTF, ecdsaKey},
		"zero deadline":     {noDeadline, ecdsaKey},
		"no chain id":       {req(noChainID), ecdsaKey},
		"no btc network":    {req(noBtcNetwork), ecdsaKey},
		"bad btc network":   {req(badBtcNetwork), ecdsaKey},
		"no sol cluster":    {req(noCluster), eddsaKey},
		"bad genesis hash":  {req(badGenesis), eddsaKey},
		"no intent":         {req(noIntent), ecdsaKey},
		"bad to":            {req(badTo), ecdsaKey},
		"no gas":            {req(noGas), ecdsaKey},
		"bad value":         {req(badValue), ecdsaKey},
		"no inputs":         {req(noInputs), ecdsaKey},
		"no blockhash":      {req(noBlockhash), eddsaKey},
		"tip over max":      {req(tipOverMax), ecdsaKey},
		"huge value":        {req(hugeValue), ecdsaKey},
		"btc sum wraps":     {req(wrapBtc), ecdsaKey},
		"btc foreign input": {req(foreignInput), ecdsaKey},
		"btc taproot input": {req(taprootInput), ecdsaKey},
		"btc bad address":   {req(badAddress), ecdsaKey},
		"btc wrong network": {req(testnetAddress), ecdsaKey},
		"btc bad txid":      {req(badTxid), ecdsaKey},
		"bad fee payer":     {req(badPayer), eddsaKey},
		"deadline huge":     {longDeadline, ecdsaKey},
	}
	for name, c := range bad {
		if err := Validate(c.r, c.k); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}

	// A wrong-curve Key is rejected for any chain the request names.
	if err := Validate(req(solTx()), ecdsaKey); !errors.Is(err, ErrCurveMismatch) {
		t.Errorf("ecdsa key on solana: %v", err)
	}
	if err := Validate(req(ethTx(), solTx()), ecdsaKey); !errors.Is(err, ErrCurveMismatch) {
		t.Errorf("one wrong-curve tx among many: %v", err)
	}
	if err := Validate(req(ethTx()), eddsaKey); !errors.Is(err, ErrCurveMismatch) {
		t.Errorf("eddsa key on ethereum: %v", err)
	}
}

func TestBuildResolvesAliasesAndDefaultsDeadline(t *testing.T) {
	spec := Spec{Reason: "r", Transactions: []TxSpec{
		{Network: "eth", Ethereum: []byte(`{"to":"AAAAAAAAAAAAAAAAAAAAAAAAAAA=","value":"1","gasLimit":"21000","maxFeePerGas":"2"}`)},
	}}
	var fromFile []TxSpec
	if err := json.Unmarshal([]byte(`[{"network":"eth","ethereum":`+string(spec.Transactions[0].Ethereum)+`}]`), &fromFile); err != nil {
		t.Fatal(err)
	}
	spec.Transactions = fromFile
	r, err := Build(spec, nets)
	if err != nil {
		t.Fatal(err)
	}
	if r.DeadlineSeconds != DefaultDeadlineSeconds {
		t.Errorf("deadline = %d", r.DeadlineSeconds)
	}
	if r.Transactions[0].GetEthereum().GetChainId() != 1 {
		t.Errorf("tx = %v; the alias must resolve to its chain id", r.Transactions[0])
	}
	if _, err := Build(Spec{Reason: "r", Transactions: []TxSpec{{Network: "nope"}}}, nets); !errors.Is(err, network.ErrUnknown) {
		t.Errorf("unknown alias: %v", err)
	}
	null := TxSpec{Network: "eth", Ethereum: []byte(`null`)}
	if _, err := Build(Spec{Reason: "r", Transactions: []TxSpec{null}}, nets); !errors.Is(err, ErrInvalid) {
		t.Errorf("null intent: %v", err)
	}
	two := TxSpec{Network: "eth", Ethereum: []byte(`{}`), Bitcoin: []byte(`{}`)}
	if _, err := Build(Spec{Reason: "r", Transactions: []TxSpec{two}}, nets); !errors.Is(err, ErrInvalid) {
		t.Errorf("two intents: %v", err)
	}
	// An intent may name its chain itself, but not one other than its alias's.
	same := TxSpec{Network: "eth", Ethereum: []byte(`{"chainId":"1"}`)}
	if _, err := Build(Spec{Reason: "r", Transactions: []TxSpec{same}}, nets); err != nil {
		t.Errorf("the alias's own chain: %v", err)
	}
	other := TxSpec{Network: "eth", Ethereum: []byte(`{"chainId":"10"}`)}
	if _, err := Build(Spec{Reason: "r", Transactions: []TxSpec{other}}, nets); !errors.Is(err, ErrInvalid) {
		t.Errorf("an intent naming another chain than its alias: %v", err)
	}
	wrongType := TxSpec{Network: "btc", Ethereum: []byte(`{}`)}
	if _, err := Build(Spec{Reason: "r", Transactions: []TxSpec{wrongType}}, nets); !errors.Is(err, ErrInvalid) {
		t.Errorf("an ethereum intent on a bitcoin Network: %v", err)
	}
}

// Two Nodes that call one chain by different aliases write the same
// request, so every Node derives the same Digest from it (ADR 0023).
func TestBuildIsTheSameUnderAnyAlias(t *testing.T) {
	intent := []byte(`{"to":"AAAAAAAAAAAAAAAAAAAAAAAAAAA=","value":"1","gasLimit":"21000","maxFeePerGas":"2"}`)
	mine, err := Build(Spec{Reason: "r", Transactions: []TxSpec{{Network: "eth", Ethereum: intent}}}, nets)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := Build(Spec{Reason: "r", Transactions: []TxSpec{{Network: "mainnet", Ethereum: intent}}},
		[]network.Network{{Alias: "mainnet", Type: "ethereum", ChainID: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(mine, theirs) {
		t.Fatalf("one chain under two aliases gave two requests:\n%v\n%v", mine, theirs)
	}
}

// A Node may accept a request only if it has a Network for every chain the
// request names; the refusal names the chain, never an alias.
func TestCheckChainsNamesTheMissingChain(t *testing.T) {
	if err := CheckChains(req(ethTx(), btcTx()), nets); err != nil {
		t.Fatalf("both chains local: %v", err)
	}
	other := ethTx()
	other.GetEthereum().ChainId = 5
	err := CheckChains(req(ethTx(), other), nets)
	if !errors.Is(err, network.ErrNoChain) || !strings.Contains(err.Error(), "ethereum chain 5") {
		t.Fatalf("chain 5: %v", err)
	}
	if err := CheckChains(req(btcTx()), nets[:1]); !errors.Is(err, network.ErrNoChain) || !strings.Contains(err.Error(), "bitcoin mainnet") {
		t.Fatalf("no bitcoin Network: %v", err)
	}
}

// Lookup derives every Digest from the request alone; a request with no
// chain identity has none and is refused, as any invalid request is.
func TestLookupDerivesDigestsFromTheRequestAlone(t *testing.T) {
	now := time.Unix(10_000, 0)
	conv := key.ConversationID("ecdsa", ecdsaKey.PublicKey)
	r := rowOf(t, 10, 0xa1, now, conv[:], req(ethTx()))
	rows := append(mintedKey(t, now), r)
	sg, err := Lookup(rows, r.MsgID)
	if err != nil {
		t.Fatal(err)
	}
	if want := "f923d8beba74c0b9fc1f63b03cab5faf4102f8c2eab0ea5ce52b75af013ab595"; len(sg.Digests) != 1 || hex.EncodeToString(sg.Digests[0]) != want {
		t.Fatalf("digests = %x, want [%s]", sg.Digests, want)
	}
	for name, tx := range map[string]*coslv1.SignTransaction{
		"no chain id":    func() *coslv1.SignTransaction { x := ethTx(); x.GetEthereum().ChainId = 0; return x }(),
		"no btc network": func() *coslv1.SignTransaction { x := btcTx(); x.GetBitcoin().Network = ""; return x }(),
		"bad cluster":    func() *coslv1.SignTransaction { x := solTx(); x.GetSolana().Cluster = "moon"; return x }(),
	} {
		bad := rowOf(t, 11, 0xb2, now, conv[:], req(tx))
		if _, err := Lookup(append(rows, bad), bad.MsgID); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

func rowOf(t *testing.T, seq uint64, id byte, created time.Time, conv []byte, r *coslv1.SignRequest) cosl.Row {
	t.Helper()
	payload, err := proto.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.Marshal(&coslv1.Envelope{Version: 1, MsgType: cosl.TypeSignRequest, Payload: payload, ConversationId: conv, CreatedAt: created.UnixNano()})
	if err != nil {
		t.Fatal(err)
	}
	return cosl.Row{Seq: seq, MsgType: cosl.TypeSignRequest, MsgID: [32]byte{id}, ConversationID: conv, CreatedAt: created.UnixNano(), Envelope: env}
}

func TestListAndInspect(t *testing.T) {
	now := time.Unix(10_000, 0)
	conv := key.ConversationID("ecdsa", ecdsaKey.PublicKey)
	open := rowOf(t, 1, 0xa1, now.Add(-10*time.Second), conv[:], req(ethTx()))
	old := rowOf(t, 2, 0xb2, now.Add(-time.Hour), conv[:], req(ethTx(), btcTx()))

	list, err := List([]cosl.Row{open, old}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Status != Open || list[1].Status != Expired || list[1].Transactions != 2 {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Conversation == "" || list[0].MsgID[:2] != "a1" {
		t.Errorf("list[0] = %+v", list[0])
	}

	d, err := Inspect([]cosl.Row{open, old}, "b2", now)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != Expired || d.Reason != "pay the invoice" || len(d.Details) != 2 {
		t.Fatalf("detail = %+v", d)
	}
	eth, btc := d.Details[0], d.Details[1]
	if eth.Chain != "ethereum chain 1" || eth.Type != "ethereum" || !strings.Contains(strings.Join(eth.Lines, "\n"), "value 1000 wei") {
		t.Errorf("eth = %+v", eth)
	}
	if want := "f923d8beba74c0b9fc1f63b03cab5faf4102f8c2eab0ea5ce52b75af013ab595"; len(eth.Digests) != 1 || eth.Digests[0] != want {
		t.Errorf("eth digests = %v, want [%s]", eth.Digests, want)
	}
	if btc.Chain != "bitcoin mainnet" || !strings.Contains(strings.Join(btc.Lines, "\n"), "fee 1000 sats") {
		t.Errorf("btc = %+v", btc)
	}

	if _, err := Inspect([]cosl.Row{open}, "ff", now); !errors.Is(err, ErrUnknown) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := Inspect([]cosl.Row{open, old}, "", now); !errors.Is(err, ErrUnknown) {
		t.Errorf("empty: %v", err)
	}
}

func acceptRow(t *testing.T, seq uint64, sender byte, created time.Time, conv []byte, request [32]byte) cosl.Row {
	t.Helper()
	payload, err := proto.Marshal(&coslv1.SignAccept{SignRequestId: request[:]})
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.Marshal(&coslv1.Envelope{Version: 1, MsgType: cosl.TypeSignAccept, Payload: payload, ConversationId: conv, CreatedAt: created.UnixNano()})
	if err != nil {
		t.Fatal(err)
	}
	return cosl.Row{Seq: seq, MsgType: cosl.TypeSignAccept, MsgID: [32]byte{0xcc, byte(seq)}, Sender: identity.ID{sender}, ConversationID: conv, CreatedAt: created.UnixNano(), Envelope: env}
}

// The proposer's request is its accept; later accepts count once each, in log
// order, only for this request and Key, and only within the deadline.
func TestAccepts(t *testing.T) {
	now := time.Unix(10_000, 0)
	conv := key.ConversationID("ecdsa", ecdsaKey.PublicKey)
	other := key.ConversationID("ecdsa", []byte{1})
	r := rowOf(t, 1, 0xa1, now, conv[:], req(ethTx())) // 300s window
	r.Sender = identity.ID{1}
	rows := []cosl.Row{
		r,
		acceptRow(t, 2, 3, now.Add(time.Second), conv[:], r.MsgID),
		acceptRow(t, 3, 3, now.Add(2*time.Second), conv[:], r.MsgID),        // duplicate
		acceptRow(t, 4, 1, now.Add(2*time.Second), conv[:], r.MsgID),        // the proposer again
		acceptRow(t, 5, 4, now.Add(2*time.Second), other[:], r.MsgID),       // another Key's Conversation
		acceptRow(t, 6, 5, now.Add(2*time.Second), conv[:], [32]byte{0x99}), // another request
		acceptRow(t, 7, 2, now.Add(time.Hour), conv[:], r.MsgID),            // past the deadline
		acceptRow(t, 8, 6, now.Add(10*time.Second), conv[:], r.MsgID),
	}
	got := Accepts(rows, r, req(ethTx()))
	want := []session.SignAccept{{Node: identity.ID{1}, Seq: 1}, {Node: identity.ID{3}, Seq: 2}, {Node: identity.ID{6}, Seq: 8}}
	if len(got) != len(want) {
		t.Fatalf("accepts = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("accepts = %+v, want %+v", got, want)
		}
	}
}

func broadcastRow(t *testing.T, seq uint64, sender byte, created time.Time, conv []byte, request [32]byte, hash byte) cosl.Row {
	t.Helper()
	payload, err := proto.Marshal(&coslv1.SignBroadcast{SignRequestId: request[:], Transactions: []*coslv1.BroadcastedTx{
		{TxHash: append(make([]byte, 31), hash)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.Marshal(&coslv1.Envelope{Version: 1, MsgType: cosl.TypeSignBroadcast, Payload: payload, ConversationId: conv, CreatedAt: created.UnixNano()})
	if err != nil {
		t.Fatal(err)
	}
	return cosl.Row{Seq: seq, MsgType: cosl.TypeSignBroadcast, MsgID: [32]byte{0xbb, byte(seq)}, Sender: identity.ID{sender}, ConversationID: conv, CreatedAt: created.UnixNano(), Envelope: env}
}

// Receipts for the request, in log order, each carrying its broadcaster, time
// and per-transaction hashes; another request's or Key's receipts are not its own.
// They show in list (as a count) and inspect (in full).
func TestBroadcastReceipts(t *testing.T) {
	now := time.Unix(10_000, 0)
	conv := key.ConversationID("ecdsa", ecdsaKey.PublicKey)
	other := key.ConversationID("ecdsa", []byte{1})
	r := rowOf(t, 1, 0xa1, now, conv[:], req(ethTx()))
	rows := []cosl.Row{
		r,
		broadcastRow(t, 2, 3, now.Add(time.Second), conv[:], r.MsgID, 1),
		broadcastRow(t, 3, 4, now.Add(2*time.Second), other[:], r.MsgID, 2),       // another Key's Conversation
		broadcastRow(t, 4, 5, now.Add(2*time.Second), conv[:], [32]byte{0x99}, 3), // another request
		broadcastRow(t, 5, 6, now.Add(3*time.Second), conv[:], r.MsgID, 1),        // a second broadcast of the same tx
		malformedBroadcast(t, 6, now.Add(4*time.Second), conv[:], r.MsgID),        // a hash of the wrong size
	}
	list, err := List(rows, now)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Broadcasts != 2 {
		t.Errorf("list broadcasts = %d, want 2", list[0].Broadcasts)
	}
	d, err := Inspect(rows, "a1", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.BroadcastReceipts) != 2 {
		t.Fatalf("receipts = %+v", d.BroadcastReceipts)
	}
	got := d.BroadcastReceipts[0]
	if got.Broadcaster != (identity.ID{3}).String() || got.At != now.Add(time.Second).UTC().Format(time.RFC3339) ||
		len(got.Transactions) != 1 || got.Transactions[0].Chain != "ethereum chain 1" ||
		got.Transactions[0].TxHash != "0000000000000000000000000000000000000000000000000000000000000001" {
		t.Errorf("receipt = %+v", got)
	}
	if d.BroadcastReceipts[1].Broadcaster != (identity.ID{6}).String() {
		t.Errorf("second receipt = %+v", d.BroadcastReceipts[1])
	}
}

func malformedBroadcast(t *testing.T, seq uint64, created time.Time, conv []byte, request [32]byte) cosl.Row {
	t.Helper()
	row := broadcastRow(t, seq, 7, created, conv, request, 1)
	payload, err := proto.Marshal(&coslv1.SignBroadcast{SignRequestId: request[:], Transactions: []*coslv1.BroadcastedTx{{TxHash: []byte{1}}}})
	if err != nil {
		t.Fatal(err)
	}
	row.Envelope, err = proto.Marshal(&coslv1.Envelope{Version: 1, MsgType: cosl.TypeSignBroadcast, Payload: payload, ConversationId: conv, CreatedAt: created.UnixNano()})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// A receipt covers one chain's transactions, saying which by index. A
// request with receipts for different chains is fully broadcast once every
// transaction is in some receipt; malformed receipts, and one that mixes
// chains, count for nothing.
func TestPartialBroadcastReceipts(t *testing.T) {
	now := time.Unix(10_000, 0)
	conv := key.ConversationID("ecdsa", ecdsaKey.PublicKey)
	op := ethTx()
	op.GetEthereum().ChainId = 10
	r := rowOf(t, 1, 0xa1, now, conv[:], req(ethTx(), ethTx(), solTx(), op))
	rcpt := func(seq uint64, txs ...*coslv1.BroadcastedTx) cosl.Row {
		return receiptRow(t, seq, now.Add(time.Duration(seq)*time.Second), conv[:], r.MsgID, txs...)
	}
	tx := func(idx uint32) *coslv1.BroadcastedTx {
		return &coslv1.BroadcastedTx{TxHash: append(make([]byte, 31), byte(idx+1)), TxIndex: idx}
	}
	status := func(rows ...cosl.Row) (Summary, Detail) {
		rows = append([]cosl.Row{r}, rows...)
		l, err := List(rows, now)
		if err != nil {
			t.Fatal(err)
		}
		d, err := Inspect(rows, "a1", now)
		if err != nil {
			t.Fatal(err)
		}
		return l[0], d
	}

	if s, _ := status(); s.FullyBroadcast || s.Broadcasts != 0 {
		t.Errorf("no receipts: %+v", s)
	}
	s, d := status(rcpt(2, tx(0), tx(1)))
	if s.FullyBroadcast || s.Broadcasts != 1 || len(d.BroadcastReceipts[0].Transactions) != 2 || d.BroadcastReceipts[0].Transactions[1].Index != 1 {
		t.Errorf("one chain: %+v %+v", s, d.BroadcastReceipts)
	}
	s, d = status(rcpt(2, tx(0), tx(1)), rcpt(3, tx(2)), rcpt(4, tx(3)))
	if !s.FullyBroadcast || s.Broadcasts != 3 || d.BroadcastReceipts[1].Transactions[0].Chain != "solana devnet" ||
		d.BroadcastReceipts[2].Transactions[0].Chain != "ethereum chain 10" {
		t.Errorf("every chain: %+v %+v", s, d.BroadcastReceipts)
	}
	// A repeat of a covered subset is one more receipt, still fully broadcast.
	if s, _ = status(rcpt(2, tx(0), tx(1)), rcpt(3, tx(2)), rcpt(4, tx(3)), rcpt(5, tx(2))); !s.FullyBroadcast || s.Broadcasts != 4 {
		t.Errorf("repeat: %+v", s)
	}

	for name, bad := range map[string][]*coslv1.BroadcastedTx{
		"empty":                 nil,
		"mixed types":           {tx(0), tx(2)},
		"mixed ethereum chains": {tx(0), tx(3)},
		"index out of range":    {tx(4)},
		"duplicate index":       {tx(0), tx(0)},
		"short hash":            {{TxHash: []byte{1}, TxIndex: 0}},
	} {
		if s, _ := status(rcpt(2, bad...)); s.Broadcasts != 0 {
			t.Errorf("%s: a malformed receipt was counted", name)
		}
	}
}

func receiptRow(t *testing.T, seq uint64, created time.Time, conv []byte, request [32]byte, txs ...*coslv1.BroadcastedTx) cosl.Row {
	t.Helper()
	payload, err := proto.Marshal(&coslv1.SignBroadcast{SignRequestId: request[:], Transactions: txs})
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.Marshal(&coslv1.Envelope{Version: 1, MsgType: cosl.TypeSignBroadcast, Payload: payload, ConversationId: conv, CreatedAt: created.UnixNano()})
	if err != nil {
		t.Fatal(err)
	}
	return cosl.Row{Seq: seq, MsgType: cosl.TypeSignBroadcast, MsgID: [32]byte{0xcc, byte(seq)}, Sender: identity.ID{byte(seq)}, ConversationID: conv, CreatedAt: created.UnixNano(), Envelope: env}
}

// A request whose Key is not on the log has no t to show: it says so instead
// of reporting zero signers needed.
func TestSummaryOfARequestWithAnUnknownKey(t *testing.T) {
	now := time.Unix(10_000, 0)
	conv := key.ConversationID("ecdsa", []byte{0x42}) // no key.create/session.complete minted it
	r := rowOf(t, 1, 0xa1, now, conv[:], req(ethTx()))
	list, err := List([]cosl.Row{r}, now)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].KeyKnown || list[0].Needed != 0 {
		t.Errorf("summary = %+v, want key unknown", list[0])
	}
}

func rawRow(t *testing.T, seq uint64, sender byte, msgType uint32, created time.Time, conv []byte, msg proto.Message) cosl.Row {
	t.Helper()
	payload, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.Marshal(&coslv1.Envelope{Version: 1, MsgType: msgType, Payload: payload, ConversationId: conv, CreatedAt: created.UnixNano()})
	if err != nil {
		t.Fatal(err)
	}
	return cosl.Row{Seq: seq, MsgType: msgType, MsgID: [32]byte{0xdd, byte(seq)}, Sender: identity.ID{sender}, ConversationID: conv, CreatedAt: created.UnixNano(), Envelope: env}
}

// mintedKey is the rows of a 2-of-2 ECDSA keygen by Nodes 1 and 2.
func mintedKey(t *testing.T, at time.Time) []cosl.Row { return mintedKeyOf(t, at, 2, 2) }

// mintedKeyOf is the rows of a t-of-n ECDSA keygen by Nodes 1 to n.
func mintedKeyOf(t *testing.T, at time.Time, tt, n byte) []cosl.Row {
	t.Helper()
	sid := bytes32(0x77)
	var nodes [][]byte
	for i := byte(1); i <= n; i++ {
		nodes = append(nodes, idBytes(i))
	}
	rows := []cosl.Row{rawRow(t, 1, 1, cosl.TypeSessionStart, at, nil,
		&coslv1.SessionStart{SessionId: sid, Curve: "ecdsa", T: uint32(tt), N: uint32(n), Participants: nodes})}
	for i := byte(1); i <= n; i++ {
		rows = append(rows, rawRow(t, uint64(1+i), i, cosl.TypeSessionComplete, at, nil,
			&coslv1.SessionComplete{SessionId: sid, Curve: "ecdsa", PublicKey: ecdsaKey.PublicKey}))
	}
	return rows
}

func bytes32(b byte) []byte { return append(make([]byte, 31), b) }

func idBytes(n byte) []byte {
	id := identity.ID{n}
	return id[:]
}

// A request's status follows what has happened to it: collecting accepts
// (open), t accepts but no Session yet (waiting), a Session open (signing),
// finished (signed) or aborted (aborted), and expired only if the deadline
// passed with no Session. A Session outlives the deadline: signed and aborted
// beat expired.
func TestRequestStatus(t *testing.T) {
	start := time.Unix(10_000, 0)
	conv := key.ConversationID("ecdsa", ecdsaKey.PublicKey)
	request := func() cosl.Row {
		r := rowOf(t, 10, 0xa1, start, conv[:], req(ethTx())) // 300s window
		r.Sender = identity.ID{1}
		return r
	}
	accept2 := func(r cosl.Row) cosl.Row { return acceptRow(t, 11, 2, start.Add(time.Second), conv[:], r.MsgID) }
	sessionStart := func(r cosl.Row) cosl.Row {
		return rawRow(t, 12, 1, cosl.TypeSessionStart, start.Add(2*time.Second), nil,
			&coslv1.SessionStart{SessionId: bytes32(0x88), Curve: "ecdsa", T: 2, N: 2, SignRequestId: r.MsgID[:],
				Participants: [][]byte{idBytes(1), idBytes(2)}})
	}
	abort := rawRow(t, 13, 2, cosl.TypeSessionAbort, start.Add(3*time.Second), nil, &coslv1.SessionAbort{SessionId: bytes32(0x88), Reason: "declined"})
	complete := rawRow(t, 13, 2, cosl.TypeSessionComplete, start.Add(3*time.Second), nil,
		&coslv1.SessionComplete{SessionId: bytes32(0x88), Curve: "ecdsa", PublicKey: ecdsaKey.PublicKey, Signatures: [][]byte{make([]byte, 65)}})

	within, after := start.Add(10*time.Second), start.Add(time.Hour)
	cases := []struct {
		name string
		rows func(r cosl.Row) []cosl.Row
		now  time.Time
		want string
	}{
		{"collecting accepts", func(r cosl.Row) []cosl.Row { return nil }, within, "open"},
		{"t accepts, no Session", func(r cosl.Row) []cosl.Row { return []cosl.Row{accept2(r)} }, within, "waiting"},
		{"Session open", func(r cosl.Row) []cosl.Row { return []cosl.Row{accept2(r), sessionStart(r)} }, within, "signing"},
		{"Session finished", func(r cosl.Row) []cosl.Row { return []cosl.Row{accept2(r), sessionStart(r), complete} }, within, "signed"},
		{"Session aborted", func(r cosl.Row) []cosl.Row { return []cosl.Row{accept2(r), sessionStart(r), abort} }, within, "aborted"},
		{"finished past the deadline", func(r cosl.Row) []cosl.Row { return []cosl.Row{accept2(r), sessionStart(r), complete} }, after, "signed"},
		{"aborted past the deadline", func(r cosl.Row) []cosl.Row { return []cosl.Row{accept2(r), sessionStart(r), abort} }, after, "aborted"},
		{"too few accepts past the deadline", func(r cosl.Row) []cosl.Row { return nil }, after, "expired"},
		{"t accepts but never started, past the deadline", func(r cosl.Row) []cosl.Row { return []cosl.Row{accept2(r)} }, after, "expired"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := request()
			rows := append(mintedKey(t, start), r)
			rows = append(rows, c.rows(r)...)
			list, err := List(rows, c.now)
			if err != nil {
				t.Fatal(err)
			}
			if got := list[0].Status; got != c.want {
				t.Errorf("List status = %q, want %q", got, c.want)
			}
			d, err := Inspect(rows, "a1", c.now)
			if err != nil {
				t.Fatal(err)
			}
			if d.Status != c.want {
				t.Errorf("Inspect status = %q, want %q", d.Status, c.want)
			}
		})
	}
}

// Listing and inspecting a Sign Request need only MsgTypes rows, so a reader
// asks the replica for those and never reads talk or round traffic: the
// answer from them alone is the whole answer.
func TestListAndInspectNeedOnlyMsgTypes(t *testing.T) {
	start := time.Unix(10_000, 0)
	within := start.Add(10 * time.Second)
	conv := key.ConversationID("ecdsa", ecdsaKey.PublicKey)
	r := rowOf(t, 10, 0xa1, start, conv[:], req(ethTx()))
	r.Sender = identity.ID{1}
	sid := bytes32(0x88)
	rows := append(mintedKey(t, start),
		rawRow(t, 4, 1, cosl.TypeAppFreeform, start, conv[:], &coslv1.AppFreeform{Text: "talk"}),
		r,
		acceptRow(t, 11, 2, start.Add(time.Second), conv[:], r.MsgID),
		rawRow(t, 12, 1, cosl.TypeSessionStart, start.Add(2*time.Second), nil,
			&coslv1.SessionStart{SessionId: sid, Curve: "ecdsa", T: 2, N: 2, SignRequestId: r.MsgID[:], Participants: [][]byte{idBytes(1), idBytes(2)}}),
		rawRow(t, 13, 2, cosl.TypeTssSession, start.Add(3*time.Second), nil, &coslv1.TssSession{SessionId: sid, Data: []byte("round")}),
		rawRow(t, 14, 2, cosl.TypeSessionComplete, start.Add(4*time.Second), nil,
			&coslv1.SessionComplete{SessionId: sid, Curve: "ecdsa", PublicKey: ecdsaKey.PublicKey, Signatures: [][]byte{make([]byte, 65)}}),
		broadcastRow(t, 15, 2, start.Add(5*time.Second), conv[:], r.MsgID, 1),
	)
	var narrow []cosl.Row
	for _, row := range rows {
		if slices.Contains(MsgTypes, row.MsgType) {
			narrow = append(narrow, row)
		}
	}
	if len(narrow) != len(rows)-2 {
		t.Fatalf("MsgTypes keeps %d of %d rows, want all but the talk and the round", len(narrow), len(rows))
	}
	list, err := List(narrow, within)
	if err != nil {
		t.Fatal(err)
	}
	if got := list[0]; got.Status != "signed" || len(got.Accepted) != 2 || got.Needed != 2 || !got.KeyKnown || got.Broadcasts != 1 {
		t.Fatalf("summary from MsgTypes rows = %+v", got)
	}
	d, err := Inspect(narrow, "a1", within)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "signed" || len(d.BroadcastReceipts) != 1 || len(d.Details) != 1 || len(d.Details[0].Digests) != 1 {
		t.Fatalf("detail from MsgTypes rows = %+v", d)
	}
}

func refuseRow(t *testing.T, seq uint64, sender byte, created time.Time, conv []byte, request [32]byte, reason, handle string) cosl.Row {
	t.Helper()
	return rawRow(t, seq, sender, cosl.TypeSignRefuse, created, conv, &coslv1.SignRefuse{SignRequestId: request[:], Reason: reason, Handle: handle})
}

func handledAccept(t *testing.T, seq uint64, sender byte, created time.Time, conv []byte, request [32]byte, handle string) cosl.Row {
	t.Helper()
	return rawRow(t, seq, sender, cosl.TypeSignAccept, created, conv, &coslv1.SignAccept{SignRequestId: request[:], Handle: handle})
}

// A Node's first accept or refusal of a request, in log order and within its
// deadline, is its stance; anything later from it is ignored. The proposer's
// stance is accept. A row with a malformed Handle, or a refusal with no
// reason, is not a stance at all.
func TestStances(t *testing.T) {
	now := time.Unix(10_000, 0)
	conv := key.ConversationID("ecdsa", ecdsaKey.PublicKey)
	rq := req(ethTx()) // 300s window
	rq.Handle = "treasurer"
	r := rowOf(t, 1, 0xa1, now, conv[:], rq)
	r.Sender = identity.ID{1}
	at := func(s int) time.Time { return now.Add(time.Duration(s) * time.Second) }
	rows := []cosl.Row{
		r,
		refuseRow(t, 2, 1, at(1), conv[:], r.MsgID, "changed my mind", ""), // the proposer
		acceptRow(t, 3, 3, at(1), conv[:], r.MsgID),
		refuseRow(t, 4, 3, at(2), conv[:], r.MsgID, "too late", ""), // 3 accepted first
		refuseRow(t, 5, 4, at(2), conv[:], r.MsgID, "unknown payee", "auditor"),
		acceptRow(t, 6, 4, at(3), conv[:], r.MsgID),                     // 4 refused first
		refuseRow(t, 7, 5, at(3), conv[:], r.MsgID, "  ", ""),           // no reason
		handledAccept(t, 8, 5, at(3), conv[:], r.MsgID, "Not A Handle"), // malformed
		handledAccept(t, 9, 5, at(4), conv[:], r.MsgID, "ops"),
		refuseRow(t, 10, 6, at(3600), conv[:], r.MsgID, "late", ""), // past the deadline
		refuseRow(t, 11, 7, at(4), conv[:], [32]byte{0x99}, "another request", ""),
	}
	got := Stances(rows, r, rq)
	want := []Stance{
		{Node: identity.ID{1}, Seq: 1, Handle: "treasurer"},
		{Node: identity.ID{3}, Seq: 3},
		{Node: identity.ID{4}, Seq: 5, Handle: "auditor", Refused: true, Reason: "unknown payee"},
		{Node: identity.ID{5}, Seq: 9, Handle: "ops"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("stances = %+v\nwant %+v", got, want)
	}
	if acc := Accepts(rows, r, rq); len(acc) != 3 || acc[2] != (session.SignAccept{Node: identity.ID{5}, Seq: 9}) {
		t.Errorf("accepts = %+v", acc)
	}
}

// A request is refused once its refusals exceed n − t, so t accepts can no
// longer exist: with a 2-of-3 Key one refusal leaves it open and two refuse
// it. A Session's status beats refused, and refused beats expired.
func TestRefusedStatus(t *testing.T) {
	start := time.Unix(10_000, 0)
	within, after := start.Add(10*time.Second), start.Add(time.Hour)
	conv := key.ConversationID("ecdsa", ecdsaKey.PublicKey)
	r := rowOf(t, 10, 0xa1, start, conv[:], req(ethTx()))
	r.Sender = identity.ID{1}
	refuse := func(seq uint64, n byte) cosl.Row {
		return refuseRow(t, seq, n, start.Add(time.Second), conv[:], r.MsgID, "no", "")
	}
	sessionStart := rawRow(t, 13, 1, cosl.TypeSessionStart, start.Add(2*time.Second), nil,
		&coslv1.SessionStart{SessionId: bytes32(0x88), Curve: "ecdsa", T: 2, N: 3, SignRequestId: r.MsgID[:],
			Participants: [][]byte{idBytes(1), idBytes(2)}})
	cases := []struct {
		name string
		rows []cosl.Row
		now  time.Time
		want string
	}{
		{"n − t refusals", []cosl.Row{refuse(11, 2)}, within, "open"},
		{"n − t + 1 refusals", []cosl.Row{refuse(11, 2), refuse(12, 3)}, within, "refused"},
		{"refused past the deadline", []cosl.Row{refuse(11, 2), refuse(12, 3)}, after, "refused"},
		{"a Session wins", []cosl.Row{refuse(11, 2), refuse(12, 3), sessionStart}, within, "signing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows := append(append(mintedKeyOf(t, start, 2, 3), r), c.rows...)
			list, err := List(rows, c.now)
			if err != nil {
				t.Fatal(err)
			}
			if got := list[0].Status; got != c.want {
				t.Errorf("List status = %q, want %q", got, c.want)
			}
			d, err := Inspect(rows, "a1", c.now)
			if err != nil {
				t.Fatal(err)
			}
			if d.Status != c.want {
				t.Errorf("Inspect status = %q, want %q", d.Status, c.want)
			}
		})
	}
}

// list and inspect show who refused beside who accepted, and the Handle of
// the agent behind the proposal, each accept and each refusal. A row written
// before Handles decodes as a person's.
func TestRefusalsAndHandlesInListAndInspect(t *testing.T) {
	start := time.Unix(10_000, 0)
	conv := key.ConversationID("ecdsa", ecdsaKey.PublicKey)
	rq := req(ethTx())
	rq.Handle = "treasurer"
	r := rowOf(t, 10, 0xa1, start, conv[:], rq)
	r.Sender = identity.ID{1}
	rows := append(mintedKeyOf(t, start, 3, 4), r,
		handledAccept(t, 11, 2, start.Add(time.Second), conv[:], r.MsgID, "ops"),
		refuseRow(t, 12, 3, start.Add(time.Second), conv[:], r.MsgID, "unknown payee", "auditor"),
		acceptRow(t, 13, 4, start.Add(time.Second), conv[:], r.MsgID))
	within := start.Add(10 * time.Second)
	list, err := List(rows, within)
	if err != nil {
		t.Fatal(err)
	}
	s := list[0]
	if s.ProposerHandle != "treasurer" || len(s.Accepted) != 3 || !slices.Equal(s.Refused, []string{(identity.ID{3}).String()}) ||
		len(s.Accepts) != 3 || s.Accepts[1].Handle != "ops" || len(s.Refusals) != 1 || s.Refusals[0].Handle != "auditor" {
		t.Fatalf("summary = %+v", s)
	}
	d, err := Inspect(rows, "a1", within)
	if err != nil {
		t.Fatal(err)
	}
	wantAccepts := []Actor{{Node: (identity.ID{1}).String(), Handle: "treasurer", Seq: 10}, {Node: (identity.ID{2}).String(), Handle: "ops", Seq: 11}, {Node: (identity.ID{4}).String(), Seq: 13}}
	if !slices.Equal(d.Accepts, wantAccepts) {
		t.Errorf("accepts = %+v, want %+v", d.Accepts, wantAccepts)
	}
	wantRefusals := []Refusal{{Actor: Actor{Node: (identity.ID{3}).String(), Handle: "auditor", Seq: 12}, Reason: "unknown payee"}}
	if !slices.Equal(d.Refusals, wantRefusals) {
		t.Errorf("refusals = %+v, want %+v", d.Refusals, wantRefusals)
	}

	old := rowOf(t, 20, 0xb2, start, conv[:], req(ethTx())) // no handle field on the wire
	d, err = Inspect([]cosl.Row{old}, "b2", within)
	if err != nil {
		t.Fatal(err)
	}
	if d.ProposerHandle != "" || len(d.Accepts) != 1 || d.Accepts[0].Handle != "" || len(d.Refusals) != 0 {
		t.Errorf("an old row = %+v", d)
	}
}

// A sign.request whose Handle is malformed is ignored as malformed: it is
// not listed, inspected or looked up.
func TestMalformedHandleRequestIsIgnored(t *testing.T) {
	now := time.Unix(10_000, 0)
	conv := key.ConversationID("ecdsa", ecdsaKey.PublicKey)
	rq := req(ethTx())
	rq.Handle = "Bad Handle"
	bad := rowOf(t, 10, 0xa1, now, conv[:], rq)
	good := rowOf(t, 11, 0xa2, now, conv[:], req(ethTx()))
	rows := append(mintedKey(t, now), bad, good)
	list, err := List(rows, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].MsgID[:2] != "a2" {
		t.Fatalf("list = %+v", list)
	}
	if d, err := Inspect(rows, "a", now); err != nil || d.MsgID[:2] != "a2" {
		t.Errorf("inspect by a prefix both share = %+v, %v; want the well-formed one", d.MsgID, err)
	}
	if _, err := Lookup(rows, bad.MsgID); !errors.Is(err, ErrUnknown) {
		t.Errorf("lookup = %v, want ErrUnknown", err)
	}
	if _, _, err := Request(rows, bad.MsgID); !errors.Is(err, ErrUnknown) {
		t.Errorf("request = %v, want ErrUnknown", err)
	}
}
