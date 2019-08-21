package vault

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/hashicorp/vault/api"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hashicorpService implements vault.vaultService and represents the Hashicorp Vault-specific functionality used by hashicorp wallets
type hashicorpService struct {
	config      HashicorpClientConfig
	secrets     []HashicorpSecretConfig
	mutex       sync.RWMutex
	client      *api.Client
	accts       []accounts.Account
	keyHandlers map[common.Address]map[accounts.URL]*hashicorpKeyHandler
}

// newHashicorpService creates a hashicorpService using the provided config
func newHashicorpService(config HashicorpWalletConfig) *hashicorpService {
	s := &hashicorpService{
		config:      config.Client,
		secrets:     config.Secrets,
		keyHandlers: make(map[common.Address]map[accounts.URL]*hashicorpKeyHandler),
	}

	return s
}

// hashicorpKeyHandler is used to relate the config for a Hashicorp-stored private key to the key itself when retrieved from the Vault
type hashicorpKeyHandler struct {
	secret HashicorpSecretConfig
	mutex  sync.RWMutex
	key    *ecdsa.PrivateKey
	cancel chan struct{}
}

// Status for a hashicorpService
const (
	open                       = "open"
	closed                     = "closed"
	hashicorpHealthcheckFailed = "Hashicorp Vault healthcheck failed"
	hashicorpUninitialized     = "Hashicorp Vault uninitialized"
	hashicorpSealed            = "Hashicorp Vault sealed"
)

var (
	hashicorpSealedErr        = errors.New(hashicorpSealed)
	hashicorpUninitializedErr = errors.New(hashicorpUninitialized)
)

type hashicorpHealthcheckErr struct {
	err error
}

func (e hashicorpHealthcheckErr) Error() string {
	return fmt.Sprintf("%v: %v", hashicorpHealthcheckFailed, e.err)
}

// status implements vault.vaultService and returns the status of the Vault API client and the unlocked status of any accounts managed by the service.
func (h *hashicorpService) status() (string, error) {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	client := h.client

	if client == nil {
		return h.withAcctStatuses(closed), nil
	}

	health, err := client.Sys().Health()

	if err != nil {
		return h.withAcctStatuses(hashicorpHealthcheckFailed), hashicorpHealthcheckErr{err: err}
	}

	if !health.Initialized {
		return h.withAcctStatuses(hashicorpUninitialized), hashicorpUninitializedErr
	}

	if health.Sealed {
		return h.withAcctStatuses(hashicorpSealed), hashicorpSealedErr
	}

	return h.withAcctStatuses(open), nil
}

// withAcctStatuses appends the locked/unlocked status of the accounts managed by the service to the provided walletStatus.
// Expects RLock to be held.
func (h *hashicorpService) withAcctStatuses(walletStatus string) string {
	status := []string{walletStatus}

	for addr, h := range h.keyHandlers {
		for _, hh := range h {
			var acctStatus string
			if hh.key == nil {
				acctStatus = "locked"
			} else {
				acctStatus = "unlocked"
			}
			status = append(status, fmt.Sprintf("%v: %v", addr.Hex(), acctStatus))
		}
	}

	return strings.Join(status, " | ")
}

// Environment variable name for Hashicorp Approle authentication credential
const (
	RoleIDEnv   = "VAULT_ROLE_ID"
	SecretIDEnv = "VAULT_SECRET_ID"
)

var (
	noHashicorpEnvSetErr  = fmt.Errorf("environment variables must be set when creating the Hashicorp client.  Set %v and %v if the Vault is configured to use Approle authentication.  Else set %v", RoleIDEnv, SecretIDEnv, api.EnvVaultToken)
	invalidApproleAuthErr = fmt.Errorf("both %v and %v must be set if using Approle authentication", RoleIDEnv, SecretIDEnv)
)

// open implements vault.vaultService creating a Vault API client from the config properties of the hashicorpService.  Once open, the client will start a loop to retrieve the account addresses for all configured secrets from the vault.  Another loop will be started to retrieve account private keys if the service has been configured to unlock all accounts by default.
//
// If Approle authentication credentials are set as environment variables, the client will attempt to authenticate with the Vault server using those credentials.  If the approle credentials are not present the Vault will attempt to use a token credential.
//
// An error is returned if the service is already open.
func (h *hashicorpService) open() error {
	if h.getClient() != nil {
		return accounts.ErrWalletAlreadyOpen
	}

	conf := api.DefaultConfig()
	conf.Address = h.config.Url

	tlsConfig := &api.TLSConfig{
		CACert:     h.config.CaCert,
		ClientCert: h.config.ClientCert,
		ClientKey:  h.config.ClientKey,
	}

	if err := conf.ConfigureTLS(tlsConfig); err != nil {
		return fmt.Errorf("error creating Hashicorp client: %v", err)
	}

	c, err := api.NewClient(conf)

	if err != nil {
		return fmt.Errorf("error creating Hashicorp client: %v", err)
	}

	roleID := os.Getenv(RoleIDEnv)
	secretID := os.Getenv(SecretIDEnv)

	if roleID == "" && secretID == "" && os.Getenv(api.EnvVaultToken) == "" {
		return noHashicorpEnvSetErr
	}

	if roleID == "" && secretID != "" || roleID != "" && secretID == "" {
		return invalidApproleAuthErr
	}

	if usingApproleAuth(roleID, secretID) {
		//authenticate the client using approle
		body := map[string]interface{}{"role_id": roleID, "secret_id": secretID}

		approle := h.config.Approle

		if approle == "" {
			approle = "approle"
		}

		resp, err := c.Logical().Write(fmt.Sprintf("auth/%s/login", approle), body)

		if err != nil {
			return err
		}

		token, err := resp.TokenID()

		c.SetToken(token)
	}

	// api.Client uses the token at VAULT_TOKEN by default so nothing extra needs to be done when not using approle
	h.mutex.Lock()
	h.client = c
	h.mutex.Unlock()

	// 10s polling interval by default
	pollingIntervalMillis := h.config.VaultPollingIntervalMillis
	if pollingIntervalMillis == 0 {
		pollingIntervalMillis = 10000
	}
	d := time.Duration(pollingIntervalMillis) * time.Millisecond

	go h.accountRetrievalLoop(time.NewTicker(d), h.config.UnlockAll)

	return nil
}

// accountRetrievalLoop periodically goes through the configured secrets and attempts to retrieve the account address
// from the Vault if not already retrieved.  If unlockAll is true the private key for each configured secret will also
// be attempted to be retrieved.
//
// The loop will continue until all accounts (and keys if unlockAll is true) are retrieved or when the ticker is
// stopped.
func (h *hashicorpService) accountRetrievalLoop(ticker *time.Ticker, unlockAll bool) {
	// attempt account retrieval once without waiting for the ticker, the loop will be used to retrieve any accounts that are left
	toRetrieve := h.tryRetrieveAccounts(h.secrets, unlockAll)

	for range ticker.C {
		if len(toRetrieve) == 0 {
			ticker.Stop()
			return
		}

		notRetrieved := h.tryRetrieveAccounts(toRetrieve, unlockAll)
		toRetrieve = notRetrieved
	}
}

// tryRetrieveAccounts attempts to retrieve the account addresses for the toRetrieve secret configs.  If unlockAll is
// true then the private key for each secret in toRetrieve will also be attempted to be retrieved.  Returns any configs
// for which the address (or address/key if unlockAll is true) could not be retrieved.
func (h *hashicorpService) tryRetrieveAccounts(toRetrieve []HashicorpSecretConfig, unlockAll bool) []HashicorpSecretConfig {
	var notRetrieved []HashicorpSecretConfig

	for _, secret := range toRetrieve {
		acct, err := h.tryRetrieveAddress(secret)

		if err != nil {
			log.Error("unable to retrieve address", "err", err)
			notRetrieved = append(notRetrieved, secret)
			continue
		}

		if unlockAll {
			if err := h.timedUnlock(acct, 0); err != nil {
				log.Error("unable to unlock acct", "acct", acct.URL.String(), "err", err)
				notRetrieved = append(notRetrieved, secret)
				continue
			}
		}
	}

	return notRetrieved
}

// TODO rlock getter for keyhandlers - will protect against colliding with the full lock applied in this method.
//  Use whenever reading from the keyhandler, shouldn't be used to store as a variable
// tryRetrieveAddress attempts to get the address component of the provided secret from the Vault, updating the
// hashicorpService's keyHandlers and returning the corresponding accounts.Account if successful.  If the address has
// already been retrieved for the given secret then the corresponding account will be returned without making another
// request to the Vault.
func (h *hashicorpService) tryRetrieveAddress(secret HashicorpSecretConfig) (accounts.Account, error) {
	// check if the address has already been retrieved for this secret
	h.mutex.RLock()
	for addr, khByUrl := range h.keyHandlers {
		for url, kh := range khByUrl {
			if kh.secret == secret {
				return accounts.Account{URL: url, Address: addr}, nil
			}
		}
	}
	h.mutex.RUnlock()

	// try and get the address from the Vault
	path := fmt.Sprintf("%s/data/%s", secret.SecretEngine, secret.AddressSecret)
	url := fmt.Sprintf("%v/v1/%v?version=%v", h.getClient().Address(), path, secret.AddressSecretVersion)

	// to parse a string url as an accounts.URL it must first be in json format
	toParse := fmt.Sprintf("\"%v\"", url)
	var parsedUrl accounts.URL

	if err := parsedUrl.UnmarshalJSON([]byte(toParse)); err != nil {
		return accounts.Account{}, fmt.Errorf("unable to parse %v to accounts.URL, err: %v", url, err.Error())
	}

	h.mutex.RLock()
	address, err := h.getAddressFromVault(secret)
	h.mutex.RUnlock()

	if err != nil {
		return accounts.Account{}, fmt.Errorf("unable to get address from Vault for %v, err: %v", url, err.Error())
	}

	acct := accounts.Account{
		Address: address,
		URL:     parsedUrl,
	}

	// update state
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if _, ok := h.keyHandlers[acct.Address]; !ok {
		h.keyHandlers[acct.Address] = make(map[accounts.URL]*hashicorpKeyHandler)
	}

	keyHandlersByUrl := h.keyHandlers[acct.Address]

	if _, ok := keyHandlersByUrl[acct.URL]; ok {
		log.Warn("Address has already been retrieved.  Ignoring.", "url", url)
		return acct, nil
	}

	keyHandlersByUrl[acct.URL] = &hashicorpKeyHandler{secret: secret}
	h.accts = append(h.accts, acct)

	return acct, nil
}

// getAddressFromVault retrieves the address component of the provided secret from the Vault.  Expects RLock to be held.
func (h *hashicorpService) getAddressFromVault(s HashicorpSecretConfig) (common.Address, error) {
	hexAddr, err := h.getSecretFromVault(s.AddressSecret, s.AddressSecretVersion, s.SecretEngine)

	if err != nil {
		return common.Address{}, err
	}

	return common.HexToAddress(hexAddr), nil
}

// getAddressFromVault retrieves the private key component of the provided secret from the Vault. Expects RLock to be held.
func (h *hashicorpService) getKeyFromVault(s HashicorpSecretConfig) (*ecdsa.PrivateKey, error) {
	hexKey, err := h.getSecretFromVault(s.PrivateKeySecret, s.PrivateKeySecretVersion, s.SecretEngine)

	if err != nil {
		return nil, err
	}

	key, err := crypto.HexToECDSA(hexKey)

	if err != nil {
		return nil, fmt.Errorf("unable to parse data from Hashicorp Vault to *ecdsa.PrivateKey: %v", err)
	}

	return key, nil
}

// getSecretFromVault retrieves a particular version of the secret 'name' from the provided secret engine. Expects RLock to be held.
func (h *hashicorpService) getSecretFromVault(name string, version int, engine string) (string, error) {
	path := fmt.Sprintf("%s/data/%s", engine, name)

	versionData := make(map[string][]string)
	versionData["version"] = []string{strconv.Itoa(version)}

	resp, err := h.client.Logical().ReadWithData(path, versionData)

	if err != nil {
		return "", fmt.Errorf("unable to get secret from Hashicorp Vault: %v", err)
	}

	if resp == nil {
		return "", fmt.Errorf("no data for secret in Hashicorp Vault")
	}

	respData, ok := resp.Data["data"].(map[string]interface{})

	if !ok {
		return "", errors.New("Hashicorp Vault response does not contain data")
	}

	if len(respData) != 1 {
		return "", errors.New("only one key/value pair is allowed in each Hashicorp Vault secret")
	}

	// get secret regardless of key in map
	var s interface{}
	for _, d := range respData {
		s = d
	}

	secret, ok := s.(string)

	if !ok {
		return "", errors.New("Hashicorp Vault response data is not in string format")
	}

	return secret, nil
}

func usingApproleAuth(roleID, secretID string) bool {
	return roleID != "" && secretID != ""
}

// close removes the client from the service preventing it from being able to retrieve data from the Vault.
func (h *hashicorpService) close() error {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	h.client = nil

	return nil
}

// accounts returns a copy of the list of signing accounts the wallet is currently aware of.
func (h *hashicorpService) accounts() []accounts.Account {
	accts := h.getAccts()
	cpy := make([]accounts.Account, len(accts))
	copy(cpy, accts)

	return cpy
}

var (
	incorrectKeyForAddrErr = errors.New("the address of the account provided does not match the address derived from the private key retrieved from the Vault.  Ensure the correct secret names and versions are specified in the node config.")
)

// getKey returns the key for the given account, making a request to the vault if the account is locked.  zeroFn is the corresponding zero function for the returned key and should be called to clean up once the key has been used.
//
// The returned key will first be validated to make sure that it is the correct key for the given address.  If not an error will be returned
func (h *hashicorpService) getKey(acct accounts.Account) (*ecdsa.PrivateKey, func(), error) {
	h.mutex.RLock()
	keyHandler, err := h.getKeyHandler(acct)
	h.mutex.RUnlock()

	if err != nil {
		return nil, func() {}, err
	}

	key, zeroFn, err := h.getKeyFromHandler(*keyHandler)

	if err != nil {
		zeroFn()
		return nil, func() {}, err
	}

	// validate that the retrieved key is correct for the provided account
	address := crypto.PubkeyToAddress(key.PublicKey)
	if !bytes.Equal(address.Bytes(), acct.Address.Bytes()) {
		zeroFn()
		return nil, func() {}, incorrectKeyForAddrErr
	}

	return key, zeroFn, nil
}

// getKeyHandler returns the associated keyHandler for the given account.  If the provided account does not specify a URL and more than one keyHandler is found for the given address, then an AmbiguousAddrErr error is returned.  Expects RLock to be held.
func (h *hashicorpService) getKeyHandler(acct accounts.Account) (*hashicorpKeyHandler, error) {
	keyHandlersByUrl, ok := h.keyHandlers[acct.Address]

	if !ok {
		return nil, accounts.ErrUnknownAccount
	}

	if (acct.URL == accounts.URL{}) && len(keyHandlersByUrl) > 1 {
		ambiguousAccounts := []accounts.Account{}

		for url, _ := range keyHandlersByUrl {
			ambiguousAccounts = append(ambiguousAccounts, accounts.Account{Address: acct.Address, URL: url})
		}

		sort.Sort(accountsByURL(ambiguousAccounts))

		err := &keystore.AmbiguousAddrError{
			Addr:    acct.Address,
			Matches: ambiguousAccounts,
		}

		return nil, err
	}

	// return the only key for this address
	if (acct.URL == accounts.URL{}) && len(keyHandlersByUrl) == 1 {
		var keyHandler *hashicorpKeyHandler

		for _, kh := range keyHandlersByUrl {
			keyHandler = kh
			break
		}

		return keyHandler, nil
	}

	keyHandler, ok := keyHandlersByUrl[acct.URL]

	if !ok {
		return nil, accounts.ErrUnknownAccount
	}

	return keyHandler, nil
}

// getKeyFromHandler uses the config in the keyHandler to return the key from the Vault along with the necessary zero function to remove the key from memory after use.
//
// If the key is already present in the keyHandler then it is simply returned along with an empty zero function without going to the Vault.
func (h *hashicorpService) getKeyFromHandler(handler hashicorpKeyHandler) (*ecdsa.PrivateKey, func(), error) {
	if key := handler.key; key != nil {
		// the account has been unlocked so we return an empty zero function to prevent the caller from being able to lock it
		return key, func() {}, nil
	}

	h.mutex.RLock()
	key, err := h.getKeyFromVault(handler.secret)
	h.mutex.RUnlock()

	if err != nil {
		return nil, func() {}, err
	}

	// zeroFn zeroes the retrieved private key
	zeroFn := func() {
		b := key.D.Bits()
		for i := range b {
			b[i] = 0
		}
		key = nil
	}

	return key, zeroFn, nil
}

// timedUnlock implements vault.vaultService unlocking the given account for the specified duration. A timeout of 0 unlocks the account until the program exits.
//
// If the account address is already unlocked for a duration, TimedUnlock extends or
// shortens the active unlock timeout. If the address was previously unlocked
// indefinitely the timeout is not altered.
func (h *hashicorpService) timedUnlock(acct accounts.Account, duration time.Duration) error {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	keyHandler, err := h.getKeyHandler(acct)

	if err != nil {
		return err
	}

	alreadyUnlocked, err := h.unlockKeyHandler(keyHandler)

	if err != nil {
		return err
	}

	if alreadyUnlocked {
		// indefinitely unlocked, do not override
		if keyHandler.cancel == nil {
			return nil
		}

		// cancel existing timed unlock
		close(keyHandler.cancel)
		keyHandler.cancel = nil
	}

	if duration == 0 {
		keyHandler.cancel = nil
	} else if duration > 0 {
		go keyHandler.timedLock(duration)
	}

	return nil
}

// unlockKeyHandler retrieves the private key from the Vault using the config in the handler and adds the key to the handler.  If the handler already has a stored key no call to the Vault is made and alreadyUnlocked is returned true.  Expects RLock to be held.
func (h *hashicorpService) unlockKeyHandler(handler *hashicorpKeyHandler) (alreadyUnlocked bool, err error) {
	if k := handler.key; k != nil && k.D.Int64() != 0 {
		return true, nil
	}

	key, err := h.getKeyFromVault(handler.secret)

	if err != nil {
		return false, err
	}

	handler.key = key

	return false, nil
}

// lock implements vault.vaultService and cancels any existing timed unlocks for the provided account and zeroes the corresponding private key if it is present
func (h *hashicorpService) lock(acct accounts.Account) error {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	keyHandler, err := h.getKeyHandler(acct)

	if err != nil {
		return err
	}

	// cancel any existing timed lock
	if keyHandler.cancel != nil {
		close(keyHandler.cancel)
		keyHandler.cancel = nil
	}

	// zero the key if it is present
	if keyHandler.key != nil {
		b := keyHandler.key.D.Bits()
		for i := range b {
			b[i] = 0
		}
		keyHandler.key = nil
	}

	return nil
}

// writeSecret implements vault.vaultService stores the provided value to a secret name at the provided secretEngine.
func (h *hashicorpService) writeSecret(name, value, secretEngine string) (string, int64, error) {
	path := fmt.Sprintf("%s/data/%s", secretEngine, name)

	data := make(map[string]interface{})
	data["data"] = map[string]interface{}{
		"secret": value,
	}

	resp, err := h.getClient().Logical().Write(path, data)

	if err != nil {
		return "", -1, fmt.Errorf("error writing secret to vault: %v", err)
	}

	v, ok := resp.Data["version"]

	if !ok {
		v = json.Number("-1")
	}

	vJson, ok := v.(json.Number)

	vInt, err := vJson.Int64()

	if err != nil {
		return "", -1, fmt.Errorf("unable to convert version in Vault response to int64: version number is %v", vJson.String())
	}

	return path, vInt, nil
}

// timedLock locks the hashicorpKeyHandler by zeroing the key after the duration.  A cancel channel is created in the hashicorpKeyHandler to enable manual cancellation of the timedLock.
func (h *hashicorpKeyHandler) timedLock(duration time.Duration) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	t := time.NewTimer(duration)
	defer t.Stop()
	h.cancel = make(chan struct{})

	select {
	case <-t.C:
		b := h.key.D.Bits()
		for i := range b {
			b[i] = 0
		}
		h.key = nil
	case <-h.cancel:
		//do nothing
	}
}

// getClient returns the client property of the hashicorpService by taking an RLock.
//
// Care should be taken not to call this within an existing Lock otherwise this a deadlock will occur.
//
// This should not be used if storing the returned client in a variable for later
// use as the fact it is a pointer means that a full Lock should be held for the
// entirety of the usage of the client.
func (h *hashicorpService) getClient() *api.Client {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	return h.client
}

// getAccts returns the accts property of the hashicorpService by taking an RLock.
//
// Care should be taken not to call this within an existing Lock otherwise this a deadlock will occur.
//
// This should not be used if storing the returned accts in a variable for later
// use as the fact it is a pointer means that a full Lock should be held for the
// entirety of the usage of the client.
func (h *hashicorpService) getAccts() []accounts.Account {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	return h.accts
}