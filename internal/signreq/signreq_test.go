package signreq

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/key"
	"defios.com/internal/network"
	coslv1 "defios.com/proto/cosl/v1"
)

const b58 = "11111111111111111111111111111111"

var (
	ethID = [32]byte{1}
	btcID = [32]byte{2}
	solID = [32]byte{3}
	nets  = []network.Network{
		{Alias: "eth", ConfigID: ethID, Type: "ethereum", ChainID: 1},
		{Alias: "btc", ConfigID: btcID, Type: "bitcoin", BitcoinNetwork: "mainnet"},
		{Alias: "sol", ConfigID: solID, Type: "solana", Cluster: "devnet"},
	}
	ecdsaKey = key.Key{Curve: "ecdsa", PublicKey: []byte{9}}
	eddsaKey = key.Key{Curve: "eddsa", PublicKey: []byte{8}}
)

func ethTx() *coslv1.SignTransaction {
	return &coslv1.SignTransaction{NetworkConfigId: ethID[:], Intent: &coslv1.SignTransaction_Ethereum{Ethereum: &coslv1.EthereumIntent{
		To: make([]byte, 20), Value: "1000", GasLimit: 21000, MaxFeePerGas: "30000000000", MaxPriorityFeePerGas: "1",
	}}}
}

func btcTx() *coslv1.SignTransaction {
	return &coslv1.SignTransaction{NetworkConfigId: btcID[:], Intent: &coslv1.SignTransaction_Bitcoin{Bitcoin: &coslv1.BitcoinIntent{
		Inputs:  []*coslv1.BitcoinInput{{Txid: strings.Repeat("ab", 32), AmountSats: 5000, ScriptPubkey: []byte{0}}},
		Outputs: []*coslv1.BitcoinOutput{{Address: "bc1qxyz", AmountSats: 4000}},
	}}}
}

func solTx() *coslv1.SignTransaction {
	return &coslv1.SignTransaction{NetworkConfigId: solID[:], Intent: &coslv1.SignTransaction_Solana{Solana: &coslv1.SolanaIntent{
		FeePayer: b58, RecentBlockhash: b58,
		Instructions: []*coslv1.SolanaInstruction{{ProgramId: b58}},
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
		if err := Validate(c.r, c.k, nets); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}

	noReason := req(ethTx())
	noReason.Reason = " "
	noDeadline := req(ethTx())
	noDeadline.DeadlineSeconds = 0
	badNonUTF := req(ethTx())
	badNonUTF.Reason = "\xff"
	unknownNet := ethTx()
	unknownNet.NetworkConfigId = []byte{7}
	wrongIntent := ethTx()
	wrongIntent.Intent = btcTx().Intent
	noIntent := &coslv1.SignTransaction{NetworkConfigId: ethID[:]}
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
		{Txid: strings.Repeat("ab", 32), AmountSats: 1 << 63, ScriptPubkey: []byte{0}},
		{Txid: strings.Repeat("ab", 32), AmountSats: 1 << 63, ScriptPubkey: []byte{0}},
	}
	badPayer := solTx()
	badPayer.GetSolana().FeePayer = "xyz"
	longDeadline := req(ethTx())
	longDeadline.DeadlineSeconds = 1 << 40

	bad := map[string]struct {
		r *coslv1.SignRequest
		k key.Key
	}{
		"no transactions": {req(), ecdsaKey},
		"blank reason":    {noReason, ecdsaKey},
		"non-utf8":        {badNonUTF, ecdsaKey},
		"zero deadline":   {noDeadline, ecdsaKey},
		"unknown network": {req(unknownNet), ecdsaKey},
		"intent mismatch": {req(wrongIntent), ecdsaKey},
		"no intent":       {req(noIntent), ecdsaKey},
		"bad to":          {req(badTo), ecdsaKey},
		"no gas":          {req(noGas), ecdsaKey},
		"bad value":       {req(badValue), ecdsaKey},
		"no inputs":       {req(noInputs), ecdsaKey},
		"no blockhash":    {req(noBlockhash), eddsaKey},
		"tip over max":    {req(tipOverMax), ecdsaKey},
		"huge value":      {req(hugeValue), ecdsaKey},
		"btc sum wraps":   {req(wrapBtc), ecdsaKey},
		"bad fee payer":   {req(badPayer), eddsaKey},
		"deadline huge":   {longDeadline, ecdsaKey},
	}
	for name, c := range bad {
		if err := Validate(c.r, c.k, nets); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}

	// A wrong-curve Key is rejected for any referenced Network.
	if err := Validate(req(solTx()), ecdsaKey, nets); !errors.Is(err, ErrCurveMismatch) {
		t.Errorf("ecdsa key on solana: %v", err)
	}
	if err := Validate(req(ethTx(), solTx()), ecdsaKey, nets); !errors.Is(err, ErrCurveMismatch) {
		t.Errorf("one wrong-curve tx among many: %v", err)
	}
	if err := Validate(req(ethTx()), eddsaKey, nets); !errors.Is(err, ErrCurveMismatch) {
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
	if string(r.Transactions[0].NetworkConfigId) != string(ethID[:]) || r.Transactions[0].GetEthereum() == nil {
		t.Errorf("tx = %v", r.Transactions[0])
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
	history := map[[32]byte]network.Network{ethID: nets[0], btcID: nets[1]}

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

	d, err := Inspect([]cosl.Row{open, old}, "b2", history, now)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != Expired || d.Reason != "pay the invoice" || len(d.Details) != 2 {
		t.Fatalf("detail = %+v", d)
	}
	eth, btc := d.Details[0], d.Details[1]
	if eth.Network != "eth" || eth.Type != "ethereum" || !strings.Contains(strings.Join(eth.Lines, "\n"), "value 1000 wei") {
		t.Errorf("eth = %+v", eth)
	}
	if want := "f923d8beba74c0b9fc1f63b03cab5faf4102f8c2eab0ea5ce52b75af013ab595"; len(eth.Digests) != 1 || eth.Digests[0] != want {
		t.Errorf("eth digests = %v, want [%s]", eth.Digests, want)
	}
	if !strings.Contains(strings.Join(btc.Lines, "\n"), "fee 1000 sats") {
		t.Errorf("btc = %+v", btc)
	}

	if _, err := Inspect([]cosl.Row{open}, "ff", history, now); !errors.Is(err, ErrUnknown) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := Inspect([]cosl.Row{open, old}, "", history, now); !errors.Is(err, ErrUnknown) {
		t.Errorf("empty: %v", err)
	}
}
