package vault

import (
	"errors"
	"fmt"
	"os"

	"defios.com/internal/flock"
)

// ErrBusy means another process holds the Vault's lock the other way: a
// passphrase change, against a shared Lock; serve or a verb using the Vault,
// against an exclusive one.
var ErrBusy = errors.New("vault: another process is using the Vault")

// Lock takes the lock on the Vault at dir. It lives beside the Vault, in
// dir.lock, so it outlives the swap of a passphrase change. Whoever uses the
// Vault under its passphrase holds it shared: a running serve for as long as
// it runs, a verb while it reads or writes. ChangePassphrase holds it
// exclusive, so none of them runs mid-change and none sees a Vault under two
// passphrases. It never waits: a lock held the other way is ErrBusy. unlock
// releases it.
func Lock(dir string, exclusive bool) (unlock func(), err error) {
	f, err := os.OpenFile(dir+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}
	if err := flock.Lock(f, exclusive, false); err != nil {
		f.Close()
		if errors.Is(err, flock.ErrWouldBlock) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("vault: locking %s: %w", dir, err)
	}
	return func() { f.Close() }, nil // closing the file releases the lock
}
