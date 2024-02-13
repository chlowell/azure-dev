// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package oneauth

import "github.com/charmbracelet/bubbles/list"

type Account struct {
	AssociatedApps []string
	DisplayName    string
	ID             string
	Username       string
}

func (Account) FilterValue() string { return "" }

func (a Account) IsZero() bool {
	return a.ID == "" && a.Username == "" && a.DisplayName == "" && len(a.AssociatedApps) == 0
}

var _ list.Item = (*Account)(nil)

type CredentialOptions struct {
	// HomeAccountID of a previously authenticated user the credential
	// should attempt to authenticate from OneAuth's cache.
	HomeAccountID string
	// NoPrompt restricts the credential to silent authentication.
	// When true, authentication fail when it requires user interaction.
	NoPrompt bool
}
