// Package outlookimap provides a small IMAP client for reading verification
// emails from Microsoft mailboxes with XOAUTH2.
package outlookimap

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"golang.org/x/net/proxy"
)

const (
	defaultIMAPServer     = "outlook.office365.com:993"
	defaultDialTimeout    = 30 * time.Second
	defaultCommandTimeout = 30 * time.Second
	defaultPollTimeout    = 2 * time.Minute
	defaultPollInterval   = 5 * time.Second
	defaultMessageWindow  = 15 * time.Minute
	defaultAuthRetries    = 3
)

// AuthMethod controls how Login authenticates after the TLS connection is open.
type AuthMethod string

const (
	// AuthAuto selects XOAUTH2 when Token is present, otherwise password login.
	AuthAuto AuthMethod = ""
	// AuthXOAUTH2 uses IMAP AUTHENTICATE XOAUTH2 with an OAuth access_token.
	AuthXOAUTH2 AuthMethod = "xoauth2"
	// AuthPassword uses the IMAP LOGIN command. Microsoft personal accounts often
	// block this unless the account is explicitly allowed to use app passwords.
	AuthPassword AuthMethod = "password"
)

// ImapConfig contains the settings needed to connect to a Microsoft mailbox.
// Token must be an OAuth2 access_token with IMAP.AccessAsUser.All permission,
// not the refresh_token that commonly starts with strings such as "M.C...".
type ImapConfig struct {
	Email      string
	Password   string
	ImapServer string
	Proxy      string // Optional SOCKS5 proxy. Supports host:port or socks5://user:pass@host:port.
	Token      string
	AuthMethod AuthMethod

	// Optional tuning knobs. Zero values use the defaults above.
	DialTimeout    time.Duration
	CommandTimeout time.Duration
	PollTimeout    time.Duration
	PollInterval   time.Duration
	MessageWindow  time.Duration
	AuthRetries    int

	// By default a matched mail is marked as Seen. Set KeepUnread to leave it
	// unread, or DeleteAfterRead to add the Deleted flag after reading it.
	KeepUnread      bool
	DeleteAfterRead bool
}

// xoauth2Client implements the SASL XOAUTH2 mechanism used by Outlook/Hotmail.
type xoauth2Client struct {
	email string
	token string
}

// XOAUTH2 returns an authentication client suitable for client.Authenticate.
func XOAUTH2(email, accessToken string) *xoauth2Client {
	return &xoauth2Client{email: email, token: accessToken}
}

func (x *xoauth2Client) Start() (string, []byte, error) {
	if x.email == "" {
		return "", nil, errors.New("imap xoauth2: email is empty")
	}
	if x.token == "" {
		return "", nil, errors.New("imap xoauth2: access token is empty")
	}

	// XOAUTH2 payload format: user=<email>^Aauth=Bearer <access_token>^A^A.
	payload := "user=" + x.email + "\x01auth=Bearer " + x.token + "\x01\x01"
	return "XOAUTH2", []byte(payload), nil
}

func (x *xoauth2Client) Next(_ []byte) ([]byte, error) {
	// On auth failure the server may send a challenge containing JSON details.
	// The XOAUTH2 client should answer with an empty response so the server can
	// finish the failed authentication cleanly.
	return []byte{}, nil
}

// Login connects to the IMAP server and authenticates with the configured
// AuthMethod. With AuthAuto, Token takes priority and uses XOAUTH2.
func (cfg *ImapConfig) Login() (*client.Client, error) {
	method, err := cfg.resolveAuthMethod()
	if err != nil {
		return nil, err
	}
	return cfg.loginWith(method)
}

// LoginXOAUTH2 connects to IMAP and authenticates with AUTHENTICATE XOAUTH2.
// Use this for Hotmail/Outlook when Token is a Microsoft OAuth access_token.
func (cfg *ImapConfig) LoginXOAUTH2() (*client.Client, error) {
	return cfg.loginWith(AuthXOAUTH2)
}

// LoginPassword connects to IMAP and authenticates with the LOGIN command.
func (cfg *ImapConfig) LoginPassword() (*client.Client, error) {
	return cfg.loginWith(AuthPassword)
}

func (cfg *ImapConfig) loginWith(method AuthMethod) (*client.Client, error) {
	if cfg.Email == "" {
		return nil, errors.New("imap login: email is empty")
	}
	if err := cfg.validateAuth(method); err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 1; attempt <= cfg.authRetries(); attempt++ {
		c, err := cfg.dialTLS()
		if err != nil {
			lastErr = err
			cfg.sleepBeforeRetry(attempt)
			continue
		}

		c.Timeout = cfg.commandTimeout()
		if err := cfg.authenticate(c, method); err != nil {
			lastErr = err
			_ = c.Logout()
			cfg.sleepBeforeRetry(attempt)
			continue
		}

		return c, nil
	}

	return nil, fmt.Errorf("imap login failed for %s: %w", cfg.Email, lastErr)
}

func (cfg *ImapConfig) authenticate(c *client.Client, method AuthMethod) error {
	switch method {
	case AuthXOAUTH2:
		return c.Authenticate(XOAUTH2(cfg.Email, cfg.Token))
	case AuthPassword:
		return c.Login(cfg.Email, cfg.Password)
	default:
		return fmt.Errorf("imap login: unsupported auth method %q", method)
	}
}

func (cfg *ImapConfig) resolveAuthMethod() (AuthMethod, error) {
	switch cfg.AuthMethod {
	case AuthAuto:
		if cfg.Token != "" {
			return AuthXOAUTH2, nil
		}
		if cfg.Password != "" {
			return AuthPassword, nil
		}
		return "", errors.New("imap login: either token or password is required")
	case AuthXOAUTH2, AuthPassword:
		return cfg.AuthMethod, nil
	default:
		return "", fmt.Errorf("imap login: unsupported auth method %q", cfg.AuthMethod)
	}
}

func (cfg *ImapConfig) validateAuth(method AuthMethod) error {
	switch method {
	case AuthXOAUTH2:
		if cfg.Token == "" {
			return errors.New("imap xoauth2 login: access token is empty")
		}
	case AuthPassword:
		if cfg.Password == "" {
			return errors.New("imap password login: password is empty")
		}
	default:
		return fmt.Errorf("imap login: unsupported auth method %q", method)
	}
	return nil
}

// GetImapMessage waits for an unread message matching mailbox, subject, and
// sender, then returns the first text body it finds. It checks the newest
// matching messages first and stops after PollTimeout.
func (cfg *ImapConfig) GetImapMessage(c *client.Client, mailboxName, subject, fromEmail string) (*string, error) {
	if c == nil {
		return nil, errors.New("imap get message: client is nil")
	}
	if mailboxName == "" {
		mailboxName = "INBOX"
	}

	if _, err := c.Select(mailboxName, false); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(cfg.pollTimeout())
	for time.Now().Before(deadline) {
		ids, err := c.Search(searchCriteria(subject, fromEmail))
		if err != nil {
			return nil, err
		}

		for i := len(ids) - 1; i >= 0; i-- {
			text, ok, err := cfg.fetchMessageText(c, ids[i])
			if err != nil {
				return nil, err
			}
			if ok {
				return &text, nil
			}
		}

		time.Sleep(cfg.pollInterval())
	}

	return nil, fmt.Errorf("email %s read message timeout", cfg.Email)
}

// KeepAlive periodically sends NOOP to keep the IMAP connection active. When
// quit is closed it logs out, which only closes this IMAP session and does not
// revoke the OAuth token.
func (cfg *ImapConfig) KeepAlive(c *client.Client, quit <-chan struct{}) {
	ticker := time.NewTicker(cfg.pollInterval())
	defer ticker.Stop()

	for {
		select {
		case <-quit:
			if c != nil {
				_ = c.Logout()
			}
			return
		case <-ticker.C:
			if c == nil {
				return
			}
			if err := c.Noop(); err != nil {
				_ = c.Logout()
				return
			}
		}
	}
}

func (cfg *ImapConfig) fetchMessageText(c *client.Client, seqNum uint32) (string, bool, error) {
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(seqNum)

	section := &imap.BodySectionName{}
	items := []imap.FetchItem{imap.FetchInternalDate, section.FetchItem()}
	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)

	go func() {
		done <- c.Fetch(seqSet, items, messages)
	}()

	msg, ok := <-messages
	if err := <-done; err != nil {
		return "", false, err
	}
	if !ok || msg == nil {
		return "", false, fmt.Errorf("imap fetch returned no message for seq %d", seqNum)
	}

	body := msg.GetBody(section)
	if body == nil {
		return "", false, fmt.Errorf("imap fetch returned no body for seq %d", seqNum)
	}

	mr, err := mail.CreateReader(body)
	if err != nil {
		return "", false, err
	}
	defer mr.Close()

	if !cfg.inMessageWindow(mr, msg.InternalDate) {
		return "", false, nil
	}

	text, ok, err := firstInlineText(mr)
	if err != nil || !ok {
		return "", false, err
	}

	if err := cfg.markAfterRead(c, seqSet); err != nil {
		return "", false, err
	}

	return text, true, nil
}

func searchCriteria(subject, fromEmail string) *imap.SearchCriteria {
	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{imap.SeenFlag}
	if subject != "" {
		criteria.Header.Set("Subject", subject)
	}
	if fromEmail != "" {
		criteria.Header.Set("From", fromEmail)
	}
	return criteria
}

func firstInlineText(mr *mail.Reader) (string, bool, error) {
	var fallback string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", false, err
		}

		inline, ok := part.Header.(*mail.InlineHeader)
		if !ok {
			continue
		}

		data, err := io.ReadAll(part.Body)
		if err != nil {
			return "", false, err
		}

		text := string(data)
		contentType, _, _ := inline.ContentType()
		if strings.EqualFold(contentType, "text/plain") {
			return text, true, nil
		}
		if fallback == "" {
			fallback = text
		}
	}

	if fallback != "" {
		return fallback, true, nil
	}
	return "", false, nil
}

func (cfg *ImapConfig) markAfterRead(c *client.Client, seqSet *imap.SeqSet) error {
	if cfg.KeepUnread {
		return nil
	}

	flag := imap.SeenFlag
	if cfg.DeleteAfterRead {
		flag = imap.DeletedFlag
	}

	item := imap.FormatFlagsOp(imap.AddFlags, true)
	return c.Store(seqSet, item, []interface{}{flag}, nil)
}

func (cfg *ImapConfig) inMessageWindow(mr *mail.Reader, internalDate time.Time) bool {
	msgTime := internalDate
	if date, err := mr.Header.Date(); err == nil {
		msgTime = date
	}
	if msgTime.IsZero() {
		return true
	}

	window := cfg.messageWindow()
	now := time.Now()
	return !msgTime.Before(now.Add(-window)) && !msgTime.After(now.Add(window))
}

func (cfg *ImapConfig) dialTLS() (*client.Client, error) {
	dialer, err := cfg.dialer()
	if err != nil {
		return nil, err
	}
	return client.DialWithDialerTLS(dialer, cfg.imapServer(), nil)
}

func (cfg *ImapConfig) dialer() (client.Dialer, error) {
	dialer := &net.Dialer{Timeout: cfg.dialTimeout()}
	if cfg.Proxy == "" {
		return dialer, nil
	}

	if strings.Contains(cfg.Proxy, "://") {
		proxyURL, err := url.Parse(cfg.Proxy)
		if err != nil {
			return nil, err
		}
		return proxy.FromURL(proxyURL, dialer)
	}

	return proxy.SOCKS5("tcp", cfg.Proxy, nil, dialer)
}

func (cfg *ImapConfig) imapServer() string {
	if cfg.ImapServer != "" {
		return cfg.ImapServer
	}
	return defaultIMAPServer
}

func (cfg *ImapConfig) dialTimeout() time.Duration {
	if cfg.DialTimeout > 0 {
		return cfg.DialTimeout
	}
	return defaultDialTimeout
}

func (cfg *ImapConfig) commandTimeout() time.Duration {
	if cfg.CommandTimeout > 0 {
		return cfg.CommandTimeout
	}
	return defaultCommandTimeout
}

func (cfg *ImapConfig) pollTimeout() time.Duration {
	if cfg.PollTimeout > 0 {
		return cfg.PollTimeout
	}
	return defaultPollTimeout
}

func (cfg *ImapConfig) pollInterval() time.Duration {
	if cfg.PollInterval > 0 {
		return cfg.PollInterval
	}
	return defaultPollInterval
}

func (cfg *ImapConfig) messageWindow() time.Duration {
	if cfg.MessageWindow > 0 {
		return cfg.MessageWindow
	}
	return defaultMessageWindow
}

func (cfg *ImapConfig) authRetries() int {
	if cfg.AuthRetries > 0 {
		return cfg.AuthRetries
	}
	return defaultAuthRetries
}

func (cfg *ImapConfig) sleepBeforeRetry(attempt int) {
	if attempt >= cfg.authRetries() {
		return
	}
	time.Sleep(time.Duration(attempt) * time.Second)
}
