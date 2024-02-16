// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

//go:build oneauth && windows

package oneauth

/*
#include <stdbool.h>
#include <stdlib.h>

// forward declaration; definition in c_funcs.go
void goLogGateway(char *s);

// Below definitions must match the ones in bridge.h exactly. We don't include
// bridge.h because doing so would make the bridge DLL a dependency of azd.exe
// and prevent distributing the DLL via embedding because Windows won't execute
// a program's entry point if its DLL dependencies are unavailable.

typedef void (*Logger)(char *);

typedef struct
{
	char *accountID;
	char *errorDescription;
	int expiresOn;
	char *token;
} WrappedAuthResult;

typedef struct
{
	char *message;
} WrappedError;

typedef struct
{
	char *id;
	char *username;
	char *displayName;
	char **associations;
	int associationsCount;
} WrappedAccount;

typedef struct
{
	WrappedAccount *accounts;
	int count;
	WrappedError *err;
} WrappedAccounts;
*/
import "C"

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/azure/azure-dev/cli/azd/internal"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"golang.org/x/sys/windows"
)

//export goLog
func goLog(s *C.char) {
	log.Print(C.GoString(s))
}

// Supported indicates whether this build includes OneAuth integration.
const Supported = true

var (
	//go:embed bridge/_build/Release/bridge.dll
	bridgeDLL []byte
	//go:embed bridge/_build/Release/bridge.dll.sha256
	bridgeChecksum string
	//go:embed bridge/_build/Release/fmt.dll
	fmtDLL []byte
	//go:embed bridge/_build/Release/fmt.dll.sha256
	fmtChecksum string

	// bridge provides access to the OneAuth API
	bridge       *windows.DLL
	authenticate *windows.Proc
	freeAccounts *windows.Proc
	freeAR       *windows.Proc
	freeError    *windows.Proc
	listAccounts *windows.Proc
	logout       *windows.Proc
	shutdown     *windows.Proc
	signInSilent *windows.Proc
	startup      *windows.Proc
)

func Shutdown() {
	if started.CompareAndSwap(true, false) {
		shutdown.Call()
	}
}

type authResult struct {
	homeAccountID string
	token         azcore.AccessToken
}

type credential struct {
	authority     string
	clientID      string
	homeAccountID string
	opts          CredentialOptions
}

// NewCredential creates a new credential that acquires tokens via OneAuth.
func NewCredential(authority, clientID string, opts CredentialOptions) (azcore.TokenCredential, error) {
	cred := &credential{
		authority:     authority,
		clientID:      clientID,
		homeAccountID: opts.HomeAccountID,
		opts:          opts,
	}
	return cred, nil
}

func (c *credential) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	ar, err := authn(c.authority, c.clientID, c.homeAccountID, strings.Join(opts.Scopes, " "), c.opts.NoPrompt)
	if err == nil {
		c.homeAccountID = ar.homeAccountID
	}
	return ar.token, err
}

func ListAccounts(clientID string) ([]Account, error) {
	err := start(clientID)
	if err != nil {
		return nil, err
	}
	p, _, _ := listAccounts.Call()
	if p == 0 {
		return nil, fmt.Errorf("couldn't list accounts")
	}
	defer freeAccounts.Call(p)
	wrapped := (*C.WrappedAccounts)(unsafe.Pointer(p))
	if wrapped.err != nil {
		return nil, fmt.Errorf(C.GoString(wrapped.err.message))
	}
	accounts := make([]Account, wrapped.count)
	for i, account := range unsafe.Slice(wrapped.accounts, wrapped.count) {
		accounts[i] = Account{
			DisplayName: C.GoString(account.displayName),
			ID:          C.GoString(account.id),
			Username:    C.GoString(account.username),
		}
		for _, assoc := range unsafe.Slice(account.associations, account.associationsCount) {
			accounts[i].AssociatedApps = append(accounts[i].AssociatedApps, C.GoString(assoc))
		}
	}
	return accounts, nil
}

func LogIn(authority, clientID, scope string) (string, error) {
	accts, err := ListAccounts(clientID)
	if err != nil {
		return "", err
	}
	choice := Account{}
	if len(accts) > 0 {
		choice, err = drawAccountPicker(accts)
		if err != nil {
			return "", err
		}
	}
	ar, err := authn(authority, clientID, choice.ID, scope, false)
	return ar.homeAccountID, err
}

func Logout(clientID string) error {
	err := start(clientID)
	if err == nil {
		logout.Call()
	}
	return err
}

// TODO: is an error ever useful? In any error case we should fall back to interactive auth.
func SignInSilently(clientID string) (string, error) {
	err := start(clientID)
	if err != nil {
		return "", err
	}
	p, _, _ := signInSilent.Call()
	if p == 0 {
		return "", fmt.Errorf("silent sign-in failed")
	}
	defer freeAR.Call(p)
	wrapped := (*C.WrappedAuthResult)(unsafe.Pointer(p))
	if wrapped.errorDescription != nil {
		return "", fmt.Errorf(C.GoString(wrapped.errorDescription))
	}
	accountID := C.GoString(wrapped.accountID)
	return accountID, err
}

func start(clientID string) error {
	if started.CompareAndSwap(false, true) {
		err := loadDLL()
		if err != nil {
			return err
		}
		clientID := unsafe.Pointer(C.CString(clientID))
		defer C.free(clientID)
		appID := unsafe.Pointer(C.CString(applicationID))
		defer C.free(appID)
		v := unsafe.Pointer(C.CString(internal.VersionInfo().Version.String()))
		defer C.free(v)
		p, _, _ := startup.Call(
			uintptr(clientID),
			uintptr(appID),
			uintptr(v),
			uintptr(unsafe.Pointer(C.goLogGateway)),
		)
		// startup returns a char* message when it fails
		if p != 0 {
			// reset started so the next call will try to start OneAuth again
			started.CompareAndSwap(true, false)
			defer freeError.Call(p)
			wrapped := (*C.WrappedError)(unsafe.Pointer(p))
			return fmt.Errorf("couldn't start OneAuth: %s", C.GoString(wrapped.message))
		}
	}
	return nil
}

func authn(authority, clientID, homeAccountID, scope string, noPrompt bool) (authResult, error) {
	res := authResult{}
	if err := start(clientID); err != nil {
		return res, err
	}
	a := unsafe.Pointer(C.CString(authority))
	defer C.free(a)
	accountID := unsafe.Pointer(C.CString(homeAccountID))
	defer C.free(accountID)
	// OneAuth always appends /.default to scopes
	scope = strings.ReplaceAll(scope, "/.default", "")
	scp := unsafe.Pointer(C.CString(scope))
	defer C.free(scp)
	allowPrompt := 1
	if noPrompt {
		allowPrompt = 0
	}
	p, _, _ := authenticate.Call(uintptr(a), uintptr(scp), uintptr(accountID), uintptr(allowPrompt))
	if p == 0 {
		// this shouldn't happen but if it did, this vague error would be better than a panic
		return res, fmt.Errorf("authentication failed")
	}
	defer freeAR.Call(p)

	wrapped := (*C.WrappedAuthResult)(unsafe.Pointer(p))
	if wrapped.errorDescription != nil {
		return res, fmt.Errorf(C.GoString(wrapped.errorDescription))
	}
	if wrapped.accountID != nil {
		res.homeAccountID = C.GoString(wrapped.accountID)
	}
	if wrapped.token != nil {
		res.token = azcore.AccessToken{
			ExpiresOn: time.Unix(int64(wrapped.expiresOn), 0),
			Token:     C.GoString(wrapped.token),
		}
	}

	return res, nil
}

// loadDLL loads the bridge DLL and its dependencies, writing them to disk if necessary.
func loadDLL() error {
	if bridge != nil {
		return nil
	}
	// cacheDir is %LocalAppData%
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(cacheDir, "azd")
	for _, dll := range []struct {
		name, checksum string
		data           []byte
	}{
		{name: "fmt.dll", checksum: fmtChecksum, data: fmtDLL},
		{name: "bridge.dll", checksum: bridgeChecksum, data: bridgeDLL},
	} {
		p := filepath.Join(dir, dll.name)
		err = writeDynamicLib(p, dll.data, dll.checksum)
		if err != nil {
			return fmt.Errorf("writing %s: %w", p, err)
		}
	}
	p := filepath.Join(dir, "bridge.dll")
	h, err := windows.LoadLibraryEx(p, 0, windows.LOAD_LIBRARY_SEARCH_DEFAULT_DIRS|windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR)
	if err == nil {
		bridge = &windows.DLL{Handle: h, Name: p}
		authenticate, err = bridge.FindProc("Authenticate")
	}
	if err == nil {
		freeAccounts, err = bridge.FindProc("FreeWrappedAccounts")
	}
	if err == nil {
		freeAR, err = bridge.FindProc("FreeWrappedAuthResult")
	}
	if err == nil {
		freeError, err = bridge.FindProc("FreeWrappedError")
	}
	if err == nil {
		listAccounts, err = bridge.FindProc("ListAccounts")
	}
	if err == nil {
		logout, err = bridge.FindProc("Logout")
	}
	if err == nil {
		shutdown, err = bridge.FindProc("Shutdown")
	}
	if err == nil {
		signInSilent, err = bridge.FindProc("SignInSilently")
	}
	if err == nil {
		startup, err = bridge.FindProc("Startup")
	}
	return err
}
