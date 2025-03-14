// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

//go:build windows

package perms

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/elastic/elastic-agent/internal/pkg/acl"
	"github.com/elastic/elastic-agent/pkg/utils"
)

// FixPermissions fixes the permissions so only SYSTEM and Administrators have access to the files in the install path
func FixPermissions(topPath string, opts ...OptFunc) error {
	o, err := newOpts(opts...)
	if err != nil {
		return err
	}

	// SYSTEM and Administrators always get permissions
	// https://support.microsoft.com/en-us/help/243330/well-known-security-identifiers-in-windows-operating-systems
	systemSID, err := windows.StringToSid(utils.SystemSID)
	if err != nil {
		return fmt.Errorf("failed to get SYSTEM SID: %w", err)
	}
	administratorsSID, err := windows.StringToSid(utils.AdministratorSID)
	if err != nil {
		return fmt.Errorf("failed to get Administrators SID: %w", err)
	}

	// https://docs.microsoft.com/en-us/windows/win32/secauthz/access-mask
	grants := make([]acl.ExplicitAccess, 0, 4)
	grants = append(grants, acl.GrantSid(0xF10F0000, systemSID))         // full control of all acl's
	grants = append(grants, acl.GrantSid(0xF10F0000, administratorsSID)) // full control of all acl's

	// user gets grant based on the mask
	userSID := administratorsSID // defaults to owned by Administrators
	if o.mask&0700 != 0 && o.ownership.UID != "" {
		userSID, err = windows.StringToSid(o.ownership.UID)
		if err != nil {
			return fmt.Errorf("failed to get user %s: %w", o.ownership.UID, err)
		}
		grants = append(grants, acl.GrantSid(uint32(((o.mask&0700)<<23)|((o.mask&0200)<<9)), userSID))
	}

	// group gets grant based on the mask
	groupSID := administratorsSID // defaults to owned by Administrators
	if o.mask&0070 != 0 && o.ownership.GID != "" {
		groupSID, err = windows.StringToSid(o.ownership.GID)
		if err != nil {
			return fmt.Errorf("failed to get group %s: %w", o.ownership.GID, err)
		}
		grants = append(grants, acl.GrantSid(uint32(((o.mask&0070)<<26)|((o.mask&0020)<<12)), groupSID))
	}

	// everyone gets grant based on the mask
	if o.mask&0007 != 0 {
		everyoneSID, err := windows.StringToSid(utils.EveryoneSID)
		if err != nil {
			return fmt.Errorf("failed to get Everyone SID: %w", err)
		}
		grants = append(grants, acl.GrantSid(uint32(((o.mask&0007)<<29)|((o.mask&0002)<<15)), everyoneSID))
	}

	// ownership can only be change to another user when running as Administrator
	isAdmin, err := utils.HasRoot()
	if err != nil {
		return fmt.Errorf("failed to determine Administrator: %w", err)
	}
	if isAdmin {
		// call to `takeOwnership` which sets the ownership information requires the current process
		// token to have the 'SeRestorePrivilege' or it's unable to adjust the ownership
		return winio.RunWithPrivileges([]string{winio.SeRestorePrivilege}, func() error {
			return filepath.WalkDir(topPath, func(name string, _ fs.DirEntry, err error) error {
				switch {
				case err == nil:
					// first level doesn't inherit
					inherit := topPath != name

					if err = acl.Apply(name, true, inherit, grants...); err != nil {
						return fmt.Errorf("apply ACL for %s failed: %w", name, err)
					}

					if userSID != nil && groupSID != nil {
						if err := acl.TakeOwnership(name, userSID, groupSID); err != nil {
							return fmt.Errorf("take ownership for %s failed: %w", name, err)
						}
					}
					return nil
				case errors.Is(err, fs.ErrNotExist):
					return nil
				default:
					return err
				}
			})
		})
	}

	// ownership cannot be changed, this will keep the ownership as it currently is but apply the ACL's
	return filepath.WalkDir(topPath, func(name string, _ fs.DirEntry, err error) error {
		switch {
		case err == nil:
			// first level doesn't inherit
			inherit := topPath != name

			if err := acl.Apply(name, true, inherit, grants...); err != nil {
				return fmt.Errorf("apply ACL for %s failed: %w", name, err)
			}

			return nil
		case errors.Is(err, fs.ErrNotExist):
			return nil
		default:
			return err
		}
	})
}
