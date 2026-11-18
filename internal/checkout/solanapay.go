package checkout

import (
	"context"
	"fmt"
	"strings"

	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
)

// SolanaPayIconPath is the square Prismaport mark on the checkout's host that wallets show next
// to a Solana Pay transaction request.
const SolanaPayIconPath = "/pay/favicon.svg"

// SolanaPayInfo answers the GET of a Solana Pay transaction request: who is asking for the
// payment.
type SolanaPayInfo struct {
	Label string `json:"label"`
	Icon  string `json:"icon"`
}

// SolanaPayTransaction answers the POST of a Solana Pay transaction request.
type SolanaPayTransaction struct {
	Transaction string `json:"transaction"`
	Message     string `json:"message"`
}

// notExpired refuses the Solana Pay request of a session past expires_at with 410
// session_expired, whatever its status: the QR a wallet scanned belongs to a closed checkout.
func (c *Checkout) notExpired(ctx context.Context, id string) error {
	ses, err := c.load(ctx, id)
	if err != nil {
		return err
	}
	if c.expired(ses) {
		return errf(service.KindGone, CodeSessionExpired, "this payment session has closed. Do not send funds to it; ask the merchant for a new payment")
	}
	return nil
}

func (c *Checkout) merchantName(ctx context.Context, ses *store.Session) (string, error) {
	m, err := c.st.GetMerchant(ctx, ses.MerchantID)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(m.Name), nil
}

// SolanaPayInfo runs every check of the POST that needs no account, so a wallet shows a problem
// before the payer approves anything, and names the merchant. It cannot know the wallet, so a
// transaction in flight refuses even the wallet it was built for.
func (c *Checkout) SolanaPayInfo(ctx context.Context, id, chainID, asset string) (SolanaPayInfo, error) {
	if err := c.notExpired(ctx, id); err != nil {
		return SolanaPayInfo{}, err
	}
	t, err := c.walletTarget(ctx, id, chainID, asset)
	if err != nil {
		return SolanaPayInfo{}, err
	}
	w, err := c.checkWallet(ctx, t)
	if err != nil {
		return SolanaPayInfo{}, err
	}
	if w.busy {
		return SolanaPayInfo{}, errInProgress
	}
	name, err := c.merchantName(ctx, t.ses)
	if err != nil {
		return SolanaPayInfo{}, err
	}
	label := "Prismaport"
	if name != "" {
		label = name + " via Prismaport"
	}
	return SolanaPayInfo{Label: label, Icon: strings.TrimRight(c.svc.View().CheckoutBaseURL, "/") + SolanaPayIconPath}, nil
}

// SolanaPay builds the same transaction as WalletTransaction for the wallet that scanned the
// request, which signs and sends it itself; the detector finds it by the session reference.
func (c *Checkout) SolanaPay(ctx context.Context, id, chainID, asset, account string) (SolanaPayTransaction, error) {
	if err := c.notExpired(ctx, id); err != nil {
		return SolanaPayTransaction{}, err
	}
	out, t, gross, err := c.buildWalletTx(ctx, id, WalletTxRequest{Chain: chainID, Asset: asset, Payer: account})
	if err != nil {
		return SolanaPayTransaction{}, err
	}
	name, err := c.merchantName(ctx, t.ses)
	if err != nil {
		return SolanaPayTransaction{}, err
	}
	msg := fmt.Sprintf("Pay %s %s", money.FormatMinPlaces(gross, t.pt.Decimals, money.USDDecimals), t.pt.Asset)
	if name != "" {
		msg += " to " + name
	}
	return SolanaPayTransaction{Transaction: out.Transaction, Message: msg}, nil
}
