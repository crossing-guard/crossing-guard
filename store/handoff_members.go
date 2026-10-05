package store

import (
	"crossing-guard/teamwire"
)

// The member directory cache (team rest-of-release plan §6.1, OD-10): the linked
// organization's active members as the server last listed them — a user id and a
// display name, and nothing else. It is replaced whole at each refresh, so a removed
// member is absent at the next one (criterion 79), and it is forgotten when the link
// ends (endHandoffLinkTx).

// TeamMember is one directory entry. Self marks the entry of this device's own user.
type TeamMember struct {
	UserID      string
	DisplayName string
	Self        bool
	RefreshedAt int64
}

// ReplaceTeamMembers replaces the directory of organizationID with the server's list,
// in one transaction that checks the device is still linked to that organization: a
// refresh in flight across an unlink writes nothing (ErrLinkChanged).
func (ix *Index) ReplaceTeamMembers(organizationID string, members []teamwire.Member, self string, at int64) error {
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	linked, err := linkedOrganizationTx(tx)
	if err != nil {
		return err
	}
	if linked != organizationID || organizationID == "" {
		return ErrLinkChanged
	}
	if _, err := tx.Exec(`DELETE FROM team_member`); err != nil {
		return err
	}
	for _, m := range members {
		if m.UserID == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO team_member(organization_id, user_id, display_name, is_self, refreshed_at) VALUES(?,?,?,?,?)
			ON CONFLICT(organization_id, user_id) DO UPDATE SET display_name=excluded.display_name, is_self=excluded.is_self, refreshed_at=excluded.refreshed_at`,
			organizationID, m.UserID, m.DisplayName, m.UserID == self, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TeamMembers lists the cached directory of one organization, by display name.
func (ix *Index) TeamMembers(organizationID string) ([]TeamMember, error) {
	rows, err := ix.db.Query(`SELECT user_id, display_name, is_self, refreshed_at FROM team_member WHERE organization_id=?
		ORDER BY display_name COLLATE NOCASE, user_id`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TeamMember
	for rows.Next() {
		var m TeamMember
		if err := rows.Scan(&m.UserID, &m.DisplayName, &m.Self, &m.RefreshedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
