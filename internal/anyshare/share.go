package anyshare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
	"unicode"
)

var ErrPickupCodeRequired = errors.New("server requires a pickup code; provide --code with 4-100 characters")

type Recipient struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name,omitempty"`
	Account string `json:"account,omitempty"`
	Path    string `json:"path,omitempty"`
}

type SharingPolicy struct {
	ValidityPeriod struct {
		Default int64 `json:"default"`
		Maximum int64 `json:"maximum"`
	} `json:"validity_period"`
	Perm struct {
		Allow   []string `json:"allow"`
		Default []string `json:"default"`
	} `json:"perm"`
	AccessTimes struct {
		Default int64 `json:"default"`
		Maximum int64 `json:"maximum"`
	} `json:"access_times"`
	MustSetPassword bool `json:"must_set_password"`
	VerifyMobile    struct {
		Enable bool `json:"enable"`
		Force  bool `json:"force"`
	} `json:"verify_mobile"`
}

type ShareOptions struct {
	Permissions  []string
	ExpiresIn    *time.Duration // nil uses the server default; zero means never.
	Code, Title  string
	Limit        *int64 // nil uses the server default; -1 means unlimited.
	VerifyMobile bool
}

func (o ShareOptions) Validate() error {
	for _, permission := range o.Permissions {
		switch permission {
		case "display", "preview", "download", "create", "modify", "delete", "cache":
		default:
			return fmt.Errorf("unknown permission %q; choose preview, download, create, modify, delete or cache", permission)
		}
	}
	if o.ExpiresIn != nil && (*o.ExpiresIn < 0 || *o.ExpiresIn > 0 && *o.ExpiresIn < time.Second) {
		return errors.New("--expires-in must be zero (never) or at least 1s; use hours such as 168h for seven days")
	}
	if o.Limit != nil && (*o.Limit == 0 || *o.Limit < -1) {
		return errors.New("--limit must be positive, or -1 for unlimited visits")
	}
	if o.Code != "" {
		if len(o.Code) < 4 || len(o.Code) > 100 || strings.IndexFunc(o.Code, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("~!%#$@-_.", r))
		}) >= 0 {
			return errors.New("--code must contain 4-100 ASCII letters, digits or ~!%#$@-_.")
		}
	}
	if len(o.Title) > 255 || strings.IndexFunc(o.Title, unicode.IsControl) >= 0 {
		return errors.New("link title must be at most 255 bytes without control characters")
	}
	return nil
}

func ValidateRecipients(recipients []Recipient) error {
	for _, recipient := range recipients {
		if recipient.Type != "user" && recipient.Type != "department" || recipient.ID == "" || len(recipient.ID) > 256 || strings.IndexFunc(recipient.ID, unicode.IsSpace) >= 0 || strings.IndexFunc(recipient.ID, unicode.IsControl) >= 0 {
			return errors.New("recipients need an explicit AnyShare user or department ID; find IDs with 'share search NAME'")
		}
	}
	return nil
}

func (c *Client) SharingPolicy(ctx context.Context, external bool) (SharingPolicy, error) {
	scope := "user"
	if external {
		scope = "anyone"
	}
	b, err := c.request(ctx, "GET", "/doc-share/v1/sharing-configuration-scope/"+scope, nil)
	var policy SharingPolicy
	if err == nil {
		err = json.Unmarshal(b, &policy)
	}
	return policy, err
}

func sharingSettings(policy SharingPolicy, opts ShareOptions) ([]string, time.Time, error) {
	if err := opts.Validate(); err != nil {
		return nil, time.Time{}, err
	}
	permissions := opts.Permissions
	if len(permissions) == 0 {
		permissions = policy.Perm.Default
	}
	if len(permissions) == 0 {
		return nil, time.Time{}, errors.New("server sharing policy has no default permissions; specify --permission")
	}
	allow := []string{"display"}
	for _, permission := range permissions {
		if !contains(policy.Perm.Allow, permission) {
			return nil, time.Time{}, fmt.Errorf("server policy does not allow sharing permission %q", permission)
		}
		if !contains(allow, permission) {
			allow = append(allow, permission)
		}
	}
	const day = 24 * time.Hour
	expires := time.Unix(0, 0).UTC()
	if opts.ExpiresIn != nil {
		if *opts.ExpiresIn > 0 {
			expires = time.Now().UTC().Add(*opts.ExpiresIn)
		}
	} else if days := policy.ValidityPeriod.Default; days != -1 {
		if days <= 0 || days > math.MaxInt64/int64(day) {
			return nil, time.Time{}, errors.New("server sharing policy has an invalid default validity period")
		}
		expires = time.Now().UTC().Add(time.Duration(days) * day)
	}
	if maximum := policy.ValidityPeriod.Maximum; maximum != -1 {
		if maximum <= 0 || expires.Unix() == 0 || expires.Sub(time.Now().UTC()) > time.Duration(min(maximum, math.MaxInt64/int64(day)))*day {
			return nil, time.Time{}, fmt.Errorf("server limits sharing validity to %d days; use a positive --expires-in", maximum)
		}
	}
	return allow, expires, nil
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

type ShareLink struct {
	ID           string     `json:"id"`
	Type         string     `json:"type"`
	URL          string     `json:"url"`
	Title        string     `json:"title"`
	Permissions  []string   `json:"permissions"`
	ExpiresAt    *time.Time `json:"expires_at"`
	Code         string     `json:"code,omitempty"`
	LimitedTimes int64      `json:"limited_times"`
	VerifyMobile bool       `json:"verify_mobile"`
}

func validLinkID(id string) bool {
	return len(id) > 0 && len(id) <= 256 && strings.IndexFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) == -1
}

func (c *Client) link(id, kind, title string, entry Entry) (ShareLink, error) {
	if !validLinkID(id) {
		return ShareLink{}, errors.New("server returned an invalid share-link ID")
	}
	// PKU's root /link route is intercepted by its landing page. Open the
	// AnyShare web portal directly, with the same item/type query as the client.
	query := url.Values{"type": {kind}, "item_id": {entry.ID}, "item_type": {entryType(entry)}}
	return ShareLink{ID: id, Type: kind, Title: title, URL: c.base + "/anyshare/zh-cn/link/" + id + "?" + query.Encode(), Permissions: []string{}, LimitedTimes: -1}, nil
}

func entryType(entry Entry) string {
	if entry.Dir {
		return "folder"
	}
	return "file"
}

// InternalLink is an authenticated navigation link, not an access grant.
func (c *Client) InternalLink(ctx context.Context, entry Entry) (ShareLink, error) {
	links, err := c.ShareLinks(ctx, entry, false)
	if err != nil {
		return ShareLink{}, err
	}
	if len(links) > 0 {
		return links[0], nil
	}
	var response struct {
		ID string `json:"id"`
	}
	if err := c.post(ctx, "/shared-link/v1/document/realname", map[string]any{"item": map[string]string{"id": entry.ID, "type": entryType(entry)}}, &response); err != nil {
		return ShareLink{}, err
	}
	return c.link(response.ID, "realname", entry.Name, entry)
}

func (c *Client) CreateExternalLink(ctx context.Context, entry Entry, opts ShareOptions) (ShareLink, error) {
	if err := opts.Validate(); err != nil {
		return ShareLink{}, err
	}
	policy, err := c.SharingPolicy(ctx, true)
	if err != nil {
		return ShareLink{}, err
	}
	allow, expires, err := sharingSettings(policy, opts)
	if err != nil {
		return ShareLink{}, err
	}
	if !entry.Dir && (contains(allow, "create") || contains(allow, "modify")) {
		return ShareLink{}, errors.New("external upload permissions (create,modify) require a folder")
	}
	if policy.MustSetPassword && opts.Code == "" {
		return ShareLink{}, ErrPickupCodeRequired
	}
	if opts.VerifyMobile && !policy.VerifyMobile.Enable {
		return ShareLink{}, errors.New("server policy does not allow phone verification")
	}
	limit := policy.AccessTimes.Default
	if opts.Limit != nil {
		limit = *opts.Limit
	}
	if limit == 0 || limit < -1 || policy.AccessTimes.Maximum != -1 && (limit == -1 || limit > policy.AccessTimes.Maximum) {
		return ShareLink{}, errors.New("visit limit exceeds the server sharing policy")
	}
	title := opts.Title
	if title == "" {
		title = entry.Name
	}
	verify := opts.VerifyMobile || policy.VerifyMobile.Force
	var response struct {
		ID string `json:"id"`
	}
	body := map[string]any{"item": map[string]any{"id": entry.ID, "type": entryType(entry), "allow": allow}, "title": title, "expires_at": expires.Format(time.RFC3339), "password": opts.Code, "verify_mobile": verify, "limited_times": limit}
	if err := c.post(ctx, "/shared-link/v1/document/anonymous", body, &response); err != nil {
		return ShareLink{}, err
	}
	link, err := c.link(response.ID, "anonymous", title, entry)
	link.Permissions, link.Code, link.LimitedTimes, link.VerifyMobile = allow, opts.Code, limit, verify
	if expires.Unix() != 0 {
		link.ExpiresAt = &expires
	}
	return link, err
}

func (c *Client) ShareLinks(ctx context.Context, entry Entry, external bool) ([]ShareLink, error) {
	kind := "realname"
	if external {
		kind = "anonymous"
	}
	links := []ShareLink{}
	seen := make(map[string]bool)
	const pageSize = 20
	for offset := 0; ; offset += pageSize {
		query := url.Values{"type": {kind}, "offset": {fmt.Sprint(offset)}, "limit": {fmt.Sprint(pageSize)}}
		data, err := c.request(ctx, "GET", "/shared-link/v1/document/"+entryType(entry)+"/"+url.PathEscape(entry.ID)+"?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var page []struct {
			ID, Title    string
			ExpiresAt    *time.Time `json:"expires_at"`
			Password     string
			LimitedTimes int64 `json:"limited_times"`
			VerifyMobile bool  `json:"verify_mobile"`
			Item         struct{ Allow []string }
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, err
		}
		for _, item := range page {
			if seen[item.ID] {
				return nil, errors.New("server repeated a share-link pagination entry")
			}
			seen[item.ID] = true
			link, err := c.link(item.ID, kind, item.Title, entry)
			if err != nil {
				return nil, err
			}
			if link.Title == "" {
				link.Title = entry.Name
			}
			if item.ExpiresAt != nil && item.ExpiresAt.Unix() != 0 {
				link.ExpiresAt = item.ExpiresAt
			}
			if item.Item.Allow != nil {
				link.Permissions = item.Item.Allow
			}
			link.Code, link.VerifyMobile = item.Password, item.VerifyMobile
			if external {
				link.LimitedTimes = item.LimitedTimes
			}
			links = append(links, link)
		}
		if len(page) < pageSize {
			return links, nil
		}
	}
}

func (c *Client) RemoveExternalLink(ctx context.Context, id string) error {
	if !validLinkID(id) {
		return errors.New("expected the link ID shown by 'link ls PATH', not a URL")
	}
	_, err := c.request(ctx, "DELETE", "/shared-link/v1/document/anonymous/"+id, nil)
	return err
}

func (c *Client) GrantShare(ctx context.Context, entry Entry, recipients []Recipient, opts ShareOptions) error {
	if len(recipients) == 0 {
		return errors.New("no recipients selected")
	}
	if err := ValidateRecipients(recipients); err != nil {
		return err
	}
	policy, err := c.SharingPolicy(ctx, false)
	if err != nil {
		return err
	}
	allow, expires, err := sharingSettings(policy, opts)
	if err != nil {
		return err
	}
	configs := make([]any, 0, len(recipients))
	for _, recipient := range recipients {
		configs = append(configs, map[string]any{"allow": allow, "deny": []string{}, "accessor": map[string]string{"id": recipient.ID, "type": recipient.Type}, "expires_at": expires.Format(time.RFC3339)})
	}
	return c.post(ctx, "/doc-share/v1/doc-perm", map[string]any{"doc_id": entry.ID, "configs": configs}, nil)
}

type SharePermission struct {
	ID            string     `json:"id"`
	Type          string     `json:"type"`
	Name          string     `json:"name"`
	Allow         []string   `json:"allow"`
	Deny          []string   `json:"deny"`
	ExpiresAt     *time.Time `json:"expires_at"`
	InheritedFrom string     `json:"inherited_from,omitempty"`
	InheritedPath string     `json:"inherited_path,omitempty"`
}

type ShareACL struct {
	Inherit     bool              `json:"inherit"`
	Permissions []SharePermission `json:"permissions"`
	raw         []json.RawMessage
}

func (c *Client) SharePermissions(ctx context.Context, id string) (ShareACL, error) {
	var response struct {
		Inherit   *bool             `json:"inherit"`
		Perminfos []json.RawMessage `json:"perminfos"`
	}
	if err := c.post(ctx, "/eacp/v1/perm2/get", map[string]string{"docid": id}, &response); err != nil {
		return ShareACL{}, err
	}
	if response.Inherit == nil || response.Perminfos == nil {
		return ShareACL{}, errors.New("server omitted the ACL inheritance flag or permission list")
	}
	acl := ShareACL{Inherit: *response.Inherit, Permissions: []SharePermission{}, raw: response.Perminfos}
	for _, raw := range response.Perminfos {
		var entry struct {
			ID            string `json:"accessorid"`
			Type          string `json:"accessortype"`
			Name          string `json:"accessorname"`
			Allow, Deny   []string
			EndTime       *int64 `json:"endtime"`
			InheritedFrom string `json:"inheritdocid"`
			InheritedPath string `json:"inheritpath"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return ShareACL{}, err
		}
		if entry.ID == "" || entry.Type == "" {
			return ShareACL{}, errors.New("server returned a permission without a recipient identity")
		}
		permission := SharePermission{ID: entry.ID, Type: entry.Type, Name: entry.Name, Allow: append([]string{}, entry.Allow...), Deny: append([]string{}, entry.Deny...), InheritedFrom: entry.InheritedFrom, InheritedPath: entry.InheritedPath}
		if entry.EndTime != nil && *entry.EndTime != -1 {
			stamp := time.UnixMicro(*entry.EndTime).UTC()
			permission.ExpiresAt = &stamp
		}
		acl.Permissions = append(acl.Permissions, permission)
	}
	return acl, nil
}

// The official revoke protocol replaces the ACL snapshot. Preserve every
// unrelated/inherited rule verbatim; no server revision/CAS is available.
func (c *Client) RevokeShare(ctx context.Context, id string, recipients []Recipient) (int, error) {
	if len(recipients) == 0 {
		return 0, errors.New("select recipients whose direct grants should be removed")
	}
	if err := ValidateRecipients(recipients); err != nil {
		return 0, err
	}
	acl, err := c.SharePermissions(ctx, id)
	if err != nil {
		return 0, err
	}
	remaining := []json.RawMessage{}
	found := make(map[string]bool)
	for i, permission := range acl.Permissions {
		remove := false
		for _, recipient := range recipients {
			if permission.InheritedFrom == "" && permission.ID == recipient.ID && permission.Type == recipient.Type {
				remove = true
				found[recipient.Type+":"+recipient.ID] = true
			}
		}
		if !remove {
			remaining = append(remaining, acl.raw[i])
		}
	}
	for _, recipient := range recipients {
		if !found[recipient.Type+":"+recipient.ID] {
			return 0, fmt.Errorf("%w: no direct %s grant for %q; inherited access must be changed at its source", ErrNotFound, recipient.Type, recipient.ID)
		}
	}
	var result struct {
		Result *int `json:"result"`
	}
	if err := c.post(ctx, "/eacp/v1/perm2/set", map[string]any{"docid": id, "inherit": acl.Inherit, "perminfos": remaining}, &result); err != nil {
		return 0, err
	}
	if result.Result == nil || *result.Result != 0 && *result.Result != 1 {
		return 0, errors.New("server omitted a recognized permission-update result")
	}
	if *result.Result == 1 {
		return 0, ErrPending
	}
	return len(acl.raw) - len(remaining), nil
}

func (c *Client) SearchRecipients(ctx context.Context, keyword string, limit int) ([]Recipient, error) {
	if strings.TrimSpace(keyword) == "" || len(keyword) > 1024 || limit < 1 || limit > 1000 {
		return nil, errors.New("recipient search needs a keyword up to 1024 bytes and --limit between 1 and 1000")
	}
	var response struct {
		Users []struct{ UserID, Name, Account, Deppath string } `json:"userinfos"`
		Deps  []struct{ DepID, Name, Path string }              `json:"depinfos"`
	}
	if err := c.post(ctx, "/eacp/v1/department/search", map[string]any{"key": keyword, "start": 0, "limit": limit}, &response); err != nil {
		return nil, err
	}
	recipients := []Recipient{}
	for _, user := range response.Users {
		recipients = append(recipients, Recipient{ID: user.UserID, Type: "user", Name: user.Name, Account: user.Account, Path: user.Deppath})
	}
	for _, department := range response.Deps {
		recipients = append(recipients, Recipient{ID: department.DepID, Type: "department", Name: department.Name, Path: department.Path})
	}
	return recipients, ValidateRecipients(recipients)
}
