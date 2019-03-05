package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/template"
	"time"

	"github.com/OpenPeeDeeP/xdg"
	"github.com/dgraph-io/badger"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/vipnode/vipnode/ethnode"
	"github.com/vipnode/vipnode/internal/pretty"
	ws "github.com/vipnode/vipnode/jsonrpc2/ws/gorilla"
	"github.com/vipnode/vipnode/pool"
	"github.com/vipnode/vipnode/pool/balance"
	"github.com/vipnode/vipnode/pool/payment"
	"github.com/vipnode/vipnode/pool/status"
	"github.com/vipnode/vipnode/pool/store"
	badgerStore "github.com/vipnode/vipnode/pool/store/badger"
	memoryStore "github.com/vipnode/vipnode/pool/store/memory"
	"golang.org/x/crypto/acme/autocert"
)

const defaultWelcomeMsg = "\x1b[31m 👑 Welcome to the demo vipnode pool! 👑 \x1b[0m You can manage your account balance here: https://vipnode.org/pool/?enode={{.NodeID}}"

// findDataDir returns a valid data dir, will create it if it doesn't
// exist.
func findDataDir(overridePath string) (string, error) {
	path := overridePath
	if path == "" {
		path = xdg.New("vipnode", "pool").DataHome()
	}
	err := os.MkdirAll(path, 0700)
	return path, err
}

func runPool(options Options) error {
	var storeDriver store.Store
	switch options.Pool.Store {
	case "memory":
		storeDriver = memoryStore.New()
		defer storeDriver.Close()
	case "persist":
		fallthrough
	case "badger":
		dir, err := findDataDir(options.Pool.DataDir)
		if err != nil {
			return err
		}
		badgerOpts := badger.DefaultOptions
		badgerOpts.Dir = dir
		badgerOpts.ValueDir = dir
		storeDriver, err = badgerStore.Open(badgerOpts)
		if err != nil {
			return err
		}
		defer storeDriver.Close()
		logger.Infof("Persistent store using badger backend: %s", dir)
	default:
		return errors.New("storage driver not implemented")
	}

	balanceStore := store.BalanceStore(storeDriver)
	var settleHandler payment.SettleHandler
	var depositGetter func(ctx context.Context) (*big.Int, error)
	if options.Pool.Contract.Addr != "" {
		// Payment contract implements NodeBalanceStore used by the balance
		// manager, but with contract awareness.
		contractPath, err := url.Parse(options.Pool.Contract.Addr)
		if err != nil {
			return err
		}

		contractAddr := common.HexToAddress(contractPath.Hostname())
		network := contractPath.Scheme
		ethclient, err := ethclient.Dial(options.Pool.Contract.RPC)
		if err != nil {
			return err
		}

		// Confirm we're on the right network
		gotNetwork, err := ethclient.NetworkID(context.Background())
		if err != nil {
			return err
		}
		if networkID := ethnode.NetworkID(int(gotNetwork.Int64())); !networkID.Is(network) {
			return ErrExplain{
				errors.New("ethereum network mismatch for payment contract"),
				fmt.Sprintf("Contract is on %q while the Contact RPC is a %q node. Please provide a Contract RPC on the same network as the contract.", network, networkID),
			}
		}

		var transactOpts *bind.TransactOpts
		if options.Pool.Contract.KeyStore != "" {
			transactOpts, err = unlockTransactor(options.Pool.Contract.KeyStore)
			if err != nil {
				return ErrExplain{
					err,
					"Failed to unlock the keystore for the contract operator wallet. Make sure the path is correct and the decryption password is set in the `KEYSTORE_PASSPHRASE` environment variable.",
				}
			}
		}

		if transactOpts == nil {
			logger.Warningf("Contract payment starting in read-only mode because --contract-keystore was not set. Withdraw and settlement attempts will fail.")
		}

		contract, err := payment.ContractPayment(storeDriver, contractAddr, ethclient, transactOpts)
		if err != nil {
			if err, ok := err.(payment.AddressMismatchError); ok {
				return ErrExplain{
					err,
					"Contract keystore must match the wallet of the contract operator. Make sure you're providing the correct keystore.",
				}
			}
			return err
		}
		balanceStore = contract
		settleHandler = contract.OpSettle

		depositGetter = func(ctx context.Context) (*big.Int, error) {
			r, err := ethclient.PendingBalanceAt(ctx, contractAddr)
			if err != nil {
				// Try again in case the connection dropped
				logger.Warningf("PoolStatus: ethclient.PendingBalanceAt failed, retrying: %s", err)
				r, err = ethclient.PendingBalanceAt(ctx, contractAddr)
			}
			if err != nil {
				logger.Errorf("PoolStatus: ethclient.PendingBalanceAt failed twice: %s", err)
			}
			return r, err
		}
	}

	// Setup balance manager
	creditPerInterval, err := pretty.ParseEther(options.Pool.Contract.Price)
	if err != nil {
		return fmt.Errorf("failed to parse contract price: %s", err)
	}
	balanceManager := balance.PayPerInterval(
		balanceStore,
		time.Minute*1, // Interval
		creditPerInterval,
	)

	if options.Pool.Contract.MinBalance != "off" {
		minBalance, err := pretty.ParseEther(options.Pool.Contract.MinBalance)
		if err != nil {
			return fmt.Errorf("failed to parse contract minimum balance: %s", err)
		}

		balanceManager.MinBalance = minBalance
	}

	// Setup welcome message template
	welcomeMsg := defaultWelcomeMsg
	if options.Pool.Contract.Welcome != "" {
		welcomeMsg = options.Pool.Contract.Welcome
	}

	welcomeTmpl, err := template.New("vipnode_welcome").Parse(welcomeMsg)
	if err != nil {
		return err
	}

	p := pool.New(storeDriver, balanceManager)
	p.MaxRequestHosts = options.Pool.MaxRequestHosts
	p.Version = fmt.Sprintf("vipnode/pool/%s", Version)
	p.ClientMessager = func(nodeID string) string {
		var buf bytes.Buffer
		err := welcomeTmpl.Execute(&buf, struct {
			NodeID string
		}{
			NodeID: nodeID,
		})
		if err != nil {
			// TODO: Should this be recoverable? What conditions would cause this?
			logger.Errorf("ClientMessager failed: %s", err)
		}
		return buf.String()
	}

	handler := &server{
		ws:     &ws.Upgrader{},
		header: http.Header{},
	}
	if options.Pool.AllowOrigin != "" {
		handler.header.Set("Access-Control-Allow-Origin", options.Pool.AllowOrigin)
	}

	if err := handler.Register("vipnode_", p); err != nil {
		return err
	}

	// Pool payment management API (optional)
	payment := &payment.PaymentService{
		NonceStore:   storeDriver,
		AccountStore: storeDriver,
		BalanceStore: balanceStore, // Proxy smart contract store if available

		WithdrawFee: func(amount *big.Int) *big.Int {
			// TODO: Adjust fee dynamically based on gas price?
			fee := big.NewInt(2500000000000000) // 0.0025 ETH
			return amount.Sub(amount, fee)
		},
		WithdrawMin: big.NewInt(5000000000000000), // 0.005 ETH
		Settle:      settleHandler,
	}
	if err := handler.Register("pool_", payment); err != nil {
		return err
	}

	// Pool status dashboard API
	dashboard := &status.PoolStatus{
		Store:           storeDriver,
		GetTotalDeposit: depositGetter,
		TimeStarted:     time.Now(),
		Version:         Version,
		CacheDuration:   time.Minute * 1,
	}
	if err := handler.Register("pool_", dashboard); err != nil {
		return err
	}

	if options.Pool.TLSHost != "" {
		if !strings.HasSuffix(":443", options.Pool.Bind) {
			logger.Warningf("Ignoring --bind value (%q) because it's not 443 and --tlshost is set.", options.Pool.Bind)
		}
		logger.Infof("Starting pool (version %s), acquiring ACME certificate and listening on: https://%s", Version, options.Pool.TLSHost)
		err := http.Serve(autocert.NewListener(options.Pool.TLSHost), handler)
		if strings.HasSuffix(err.Error(), "bind: permission denied") {
			err = ErrExplain{err, "Hosting a pool with autocert requires CAP_NET_BIND_SERVICE capability permission to bind on low-numbered ports. See: https://superuser.com/questions/710253/allow-non-root-process-to-bind-to-port-80-and-443/892391"}
		}
		return err
	}
	logger.Infof("Starting pool (version %s), listening on: %s", Version, options.Pool.Bind)
	return http.ListenAndServe(options.Pool.Bind, handler)
}

func unlockTransactor(keystorePath string) (*bind.TransactOpts, error) {
	pw := os.Getenv("KEYSTORE_PASSPHRASE")
	r, err := os.Open(keystorePath)
	if err != nil {
		return nil, err
	}
	return bind.NewTransactor(r, pw)
}
