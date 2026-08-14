package intermediary

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/svpchain/svpdt"

	"github.com/svpchain/svpchain-research-agent/internal/agentchain"
)

func nowUnix() int64 { return time.Now().Unix() }

func drawNonce() ([16]byte, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nonce, fmt.Errorf("draw nonce: %w", err)
	}
	return nonce, nil
}

// toDTCoins converts one wire coin into the credential library's form. The
// library keeps no dependency on cosmossdk.io/math so it stays portable to the
// TypeScript and Python verifiers, so the conversion happens at this boundary.
func toDTCoins(c agentchain.Coin) (svpdt.Coins, error) {
	amount, ok := new(big.Int).SetString(strings.TrimSpace(c.Amount), 10)
	if !ok {
		return nil, fmt.Errorf("amount %q is not a base-10 integer", c.Amount)
	}
	return svpdt.NewCoins(svpdt.Coin{Denom: strings.TrimSpace(c.Denom), Amount: amount})
}

func coinsView(c svpdt.Coins) []agentchain.Coin {
	if len(c) == 0 {
		return nil
	}
	out := make([]agentchain.Coin, 0, len(c))
	for _, coin := range c {
		out = append(out, agentchain.Coin{Denom: coin.Denom, Amount: coin.Amount.String()})
	}
	return out
}
