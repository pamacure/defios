package serve

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"defios.com/internal/btcrpc"
	"defios.com/internal/chainfill"
	"defios.com/internal/cosl"
	"defios.com/internal/errlog"
	"defios.com/internal/ethrpc"
	"defios.com/internal/key"
	"defios.com/internal/network"
	"defios.com/internal/outbox"
	"defios.com/internal/secret"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
	"defios.com/internal/solrpc"
	coslv1 "defios.com/proto/cosl/v1"
)

// What follows a Sign Request once it is signed
// (docs/adr/0045-a-signing-session-refreshes-broadcasts-and-confirms.md):
// the fresh chain fields its start carries, its automatic broadcast, and
// watching each transaction this Node broadcast until it has an outcome.

const (
	// autoBroadcastWait is how long a signer that did not start the Session
	// waits for its starter's receipt before broadcasting itself.
	autoBroadcastWait = 30 * time.Second
	// autoBroadcastWithin is how long after its complete a request is
	// broadcast automatically, so a restart does not resubmit an old one.
	autoBroadcastWithin = 10 * time.Minute
	// settleTick is how often settling looks again with no commit to wake it:
	// an automatic broadcast that waits on a clock.
	settleTick = 5 * time.Second

	ethereumWithin = 10 * time.Minute
	bitcoinWithin  = 24 * time.Hour
	// solanaWithin bounds watching a Solana transaction whose blockhash
	// still reads valid: it lives about 90 seconds.
	solanaWithin = 5 * time.Minute
)

// Poll intervals, by chain type; a test shortens them.
var settlePolls = map[string]time.Duration{
	network.Solana:   2 * time.Second,
	network.Ethereum: 4 * time.Second,
	network.Bitcoin:  time.Minute,
}

// refreshSign is the Session layer's Config.Refresh: the fresh chain fields
// of the request's transactions, read from this Node's Networks for its Key.
// A chain it cannot read keeps its proposed fields, which it logs. It reads
// the store it is given, as signInfoFrom does.
func (s *server) refreshSign(store *cosl.Store) func(context.Context, [32]byte) *coslv1.SignRefresh {
	return func(ctx context.Context, id [32]byte) *coslv1.SignRefresh {
		ref, errs := s.refresh(ctx, store, id)
		for _, err := range errs {
			s.opts.Logf("serve: sign request %x: %v; signing with its proposed fields", id[:4], err)
		}
		return ref
	}
}

func (s *server) refresh(ctx context.Context, store *cosl.Store, id [32]byte) (*coslv1.SignRefresh, []error) {
	rows, err := store.Log(cosl.Query{MsgTypes: signreq.LookupMsgTypes})
	if err != nil {
		return nil, []error{err}
	}
	req, conv, err := signreq.Request(rows, id)
	if err != nil {
		return nil, []error{err}
	}
	info, err := signreq.Lookup(rows, id)
	if err != nil {
		return nil, []error{err}
	}
	e, err := s.load(store, conv)
	if err != nil {
		return nil, []error{err}
	}
	nets, err := e.Networks()
	if err != nil {
		return nil, []error{err}
	}
	r := s.resolver()
	k := key.Key{Curve: info.Curve, PublicKey: info.PublicKey}
	return chainfill.Refresh(ctx, k, nets, req, func(n network.Network) ([]string, error) {
		return endpoints(r, nets, n.Chain())
	})
}

func (s *server) resolver() *secret.Resolver {
	return &secret.Resolver{Group: s.opts.Alias, Getenv: s.opts.Getenv, Vault: s.opts.Secrets}
}

// settler is keepSettled's state between wakes.
type settler struct {
	s     *server
	store *cosl.Store
	ob    *outbox.Outbox
	now   func() time.Time
	errs  *errlog.Reporter
	wg    sync.WaitGroup

	mu sync.Mutex
	// tried are the requests this run broadcast automatically, or began to.
	tried map[[32]byte]bool
	// watching are the transactions being watched, by request and index;
	// watched, the receipts already watched to an outcome this run.
	watching map[txRef]bool
	watched  map[receiptRef]bool
}

type txRef struct {
	request [32]byte
	index   uint32
}

type receiptRef struct {
	txRef
	seq uint64
}

// keepSettled broadcasts the requests this Node signed and watches what it
// broadcast to an outcome, waking at each commit and every settleTick,
// until ctx ends. It waits for the broadcasts and watches it began.
func (s *server) keepSettled(ctx context.Context, store *cosl.Store, ob *outbox.Outbox) {
	st := &settler{
		s: s, store: store, ob: ob, now: time.Now,
		errs:  errlog.New(s.opts.Logf, "serve: settling sign requests"),
		tried: map[[32]byte]bool{}, watching: map[txRef]bool{}, watched: map[receiptRef]bool{},
	}
	defer st.wg.Wait()
	tick := time.NewTicker(settleTick)
	defer tick.Stop()
	for {
		changes := store.Changed()
		st.errs.Report(st.wake(ctx))
		select {
		case <-ctx.Done():
			return
		case <-changes:
		case <-tick.C:
		}
	}
}

// wake looks at every signed request: it broadcasts one that is due, and
// watches each transaction this Node broadcast that has no outcome yet.
func (st *settler) wake(ctx context.Context) error {
	rows, err := st.store.Log(cosl.Query{MsgTypes: signreq.MsgTypes})
	if err != nil {
		return err
	}
	signed, err := session.SignedSessions(rows, SignInfoOf(rows))
	if err != nil {
		return err
	}
	me := st.s.opts.Node.NodeID
	for _, ss := range signed {
		if st.due(rows, ss) {
			st.wg.Go(func() { st.autoBroadcast(ctx, ss.Request) })
		}
		open, err := signreq.Unsettled(rows, ss.Request, me)
		if err != nil {
			continue // not a request this Node reads: nothing it broadcast
		}
		for _, tx := range open {
			ref := receiptRef{txRef{ss.Request, tx.Index}, tx.ReceiptSeq}
			st.mu.Lock()
			start := !st.watching[ref.txRef] && !st.watched[ref]
			if start {
				st.watching[ref.txRef] = true
			}
			st.mu.Unlock()
			if start {
				st.wg.Go(func() { st.watch(ctx, rows, ref, tx) })
			}
		}
	}
	return nil
}

// due is whether this Node broadcasts ss's request now: it signed it, no
// one has broadcast it or tried to, it completed within
// autoBroadcastWithin, and this Node started it or its starter has had
// autoBroadcastWait to. It is marked tried, so it is broadcast at most once.
func (st *settler) due(rows []cosl.Row, ss session.SignedSession) bool {
	me := st.s.opts.Node.NodeID
	age := st.now().Sub(ss.Completed)
	if st.s.opts.ManualBroadcast || !slices.Contains(ss.Signers, me) || age > autoBroadcastWithin || (ss.Starter != me && age < autoBroadcastWait) {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.tried[ss.Request] || signreq.Answered(rows, ss.Request) {
		return false
	}
	st.tried[ss.Request] = true
	return true
}

func (st *settler) autoBroadcast(ctx context.Context, id [32]byte) {
	v, err := st.s.broadcast(ctx, st.store, st.ob, id)
	switch {
	case err != nil:
		st.s.opts.Logf("serve: broadcasting sign request %x: %v", id[:4], err)
	case v.Failed():
		for _, c := range v.Chains {
			if c.Error != "" {
				st.s.opts.Logf("serve: sign request %x: %s", id[:4], c.Error)
			}
		}
	}
}

// watch follows one transaction this Node broadcast until it has an
// outcome, which it commits. Ending ctx stops it with nothing written: a
// restart watches it again.
func (st *settler) watch(ctx context.Context, rows []cosl.Row, ref receiptRef, tx signreq.UnsettledTx) {
	done := false
	defer func() {
		st.mu.Lock()
		delete(st.watching, ref.txRef)
		if done {
			st.watched[ref] = true
		}
		st.mu.Unlock()
	}()
	req, conv, err := signreq.SignedRequest(rows, ref.request)
	if err != nil {
		st.s.opts.Logf("serve: watching sign request %x: %v", ref.request[:4], err)
		return
	}
	t := req.Transactions[tx.Index]
	c := signreq.ChainOf(t)
	e, err := st.s.load(st.store, conv)
	if err != nil {
		st.s.opts.Logf("serve: watching sign request %x: %v", ref.request[:4], err)
		return
	}
	nets, err := e.Networks()
	if err != nil {
		st.s.opts.Logf("serve: watching sign request %x: %v", ref.request[:4], err)
		return
	}
	n, err := network.Find(nets, c)
	if err != nil {
		st.s.opts.Logf("serve: watching sign request %x: %v", ref.request[:4], err)
		return
	}
	w := watcher{
		urls:  func() ([]string, error) { return endpoints(st.s.resolver(), nets, c) },
		since: time.Unix(0, tx.ReceiptAt), now: st.now, poll: settlePolls[c.Type],
		net: n, chain: c, tx: t, id: tx.TxHash,
	}
	if c.Type == network.Ethereum {
		info, err := signreq.Lookup(rows, ref.request)
		if err == nil {
			w.sender, err = key.EthereumAddress(info.PublicKey)
		}
		if err != nil {
			st.s.opts.Logf("serve: watching sign request %x: %v", ref.request[:4], err)
			return
		}
	}
	o, err := w.run(ctx)
	if err != nil {
		if ctx.Err() == nil {
			st.s.opts.Logf("serve: watching transaction %d of sign request %x: %v", tx.Index, ref.request[:4], err)
		}
		return
	}
	o.SignRequestId, o.TxIndex, o.TxHash = ref.request[:], tx.Index, tx.TxHash
	if _, err := st.s.commitOutcome(ctx, st.ob, conv, o); err != nil {
		st.s.opts.Logf("serve: recording the outcome of transaction %d of sign request %x: %v", tx.Index, ref.request[:4], err)
		return
	}
	done = true
}

// watcher polls one transaction's chain for what became of it.
type watcher struct {
	urls  func() ([]string, error)
	since time.Time // when it was broadcast
	now   func() time.Time
	poll  time.Duration
	net   network.Network
	chain network.Chain
	tx    *coslv1.SignTransaction
	id    []byte // its transaction id
	// sender is an ethereum transaction's: the Key's address.
	sender string
}

// errPending is a chain that cannot say yet.
var errPending = errors.New("not settled yet")

// run polls until the transaction has an outcome, or ctx ends. A read that
// fails, on every endpoint, is tried again at the next poll.
func (w watcher) run(ctx context.Context) (*coslv1.SignOutcome, error) {
	for {
		o, err := w.check(ctx)
		if err == nil {
			return o, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(w.poll):
		}
	}
}

// check reads the transaction's state once: an outcome, or errPending (or
// a read error) while it has none.
func (w watcher) check(ctx context.Context) (*coslv1.SignOutcome, error) {
	urls, err := w.urls()
	if err != nil {
		return nil, err
	}
	switch w.chain.Type {
	case network.Ethereum:
		return w.checkEthereum(ctx, urls)
	case network.Solana:
		return w.checkSolana(ctx, urls)
	case network.Bitcoin:
		return w.checkBitcoin(ctx, urls)
	}
	return nil, fmt.Errorf("no way to watch a %s transaction", w.chain.Type)
}

func (w watcher) checkEthereum(ctx context.Context, urls []string) (*coslv1.SignOutcome, error) {
	var out *coslv1.SignOutcome
	err := ethrpc.Use(ctx, urls, w.chain.ChainID, func(e ethrpc.Endpoint) error {
		r, err := e.TransactionReceipt(ctx, [32]byte(w.id))
		if err != nil {
			return err
		}
		if r != nil {
			head, err := e.BlockNumber(ctx)
			if err != nil {
				return err
			}
			if head+1 < r.Block+w.net.Depth() {
				return nil // mined, not yet deep enough
			}
			out = &coslv1.SignOutcome{Result: signreq.ResultConfirmed, Block: r.Block}
			if !r.OK {
				out.Result, out.Detail = signreq.ResultReverted, "the transaction reverted"
			}
			return nil
		}
		eth := w.tx.GetEthereum()
		next, err := e.Nonce(ctx, w.sender)
		if err != nil {
			return err
		}
		switch {
		case next > eth.Nonce:
			out = &coslv1.SignOutcome{Result: signreq.ResultDropped, Detail: fmt.Sprintf("nonce %d was used by another transaction", eth.Nonce)}
		case w.now().Sub(w.since) > ethereumWithin:
			out = &coslv1.SignOutcome{Result: signreq.ResultDropped, Detail: fmt.Sprintf("not mined within %s of broadcasting", ethereumWithin)}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errPending
	}
	return out, nil
}

func (w watcher) checkSolana(ctx context.Context, urls []string) (*coslv1.SignOutcome, error) {
	genesis := w.chain.GenesisHash
	if genesis == "" {
		genesis = w.net.GenesisHash
	}
	var out *coslv1.SignOutcome
	err := solrpc.Use(ctx, urls, genesis, func(e solrpc.Endpoint) error {
		st, err := e.SignatureStatus(ctx, w.id)
		if err != nil {
			return err
		}
		if st != nil {
			if st.Confirmation != "confirmed" && st.Confirmation != "finalized" {
				return nil // processed: not yet confirmed
			}
			out = &coslv1.SignOutcome{Result: signreq.ResultConfirmed, Block: st.Slot}
			if st.Err != "" {
				out.Result, out.Detail = signreq.ResultReverted, st.Err
			}
			return nil
		}
		valid, err := e.BlockhashValid(ctx, w.tx.GetSolana().RecentBlockhash)
		if err != nil {
			return err
		}
		if !valid {
			// It can no longer land; look once more, in case it did since.
			if st, err := e.SignatureStatus(ctx, w.id); err != nil || st != nil {
				return err // seen after all: the next poll settles it
			}
			out = &coslv1.SignOutcome{Result: signreq.ResultDropped, Detail: "its blockhash expired before it landed"}
			return nil
		}
		if w.now().Sub(w.since) > solanaWithin {
			out = &coslv1.SignOutcome{Result: signreq.ResultDropped, Detail: fmt.Sprintf("not confirmed within %s of broadcasting", solanaWithin)}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errPending
	}
	return out, nil
}

func (w watcher) checkBitcoin(ctx context.Context, urls []string) (*coslv1.SignOutcome, error) {
	var out *coslv1.SignOutcome
	err := btcrpc.Use(ctx, urls, w.chain.BitcoinNetwork, func(e btcrpc.Endpoint) error {
		height, err := e.Mined(ctx, [32]byte(w.id))
		if err != nil {
			return err
		}
		switch {
		case height > 0:
			out = &coslv1.SignOutcome{Result: signreq.ResultConfirmed, Block: height}
		case w.now().Sub(w.since) > bitcoinWithin:
			out = &coslv1.SignOutcome{Result: signreq.ResultDropped, Detail: fmt.Sprintf("not confirmed within %s of broadcasting", bitcoinWithin)}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errPending
	}
	return out, nil
}
