# imapclient

Small Go helper for reading Microsoft Outlook/Hotmail verification emails over IMAP with XOAUTH2.

## Install

Before publishing this repository, replace the module path with your real GitHub path:

```powershell
go mod edit -module github.com/<your-github-username>/imapclient
go mod tidy
```

Then other projects can use it with:

```powershell
go get github.com/<your-github-username>/imapclient
```

## Usage

```go
package main

import (
    "fmt"
    "log"
    "time"

    "github.com/<your-github-username>/imapclient"
)

func main() {
    cfg := imapclient.ImapConfig{
        Email:        "user@hotmail.com",
        Token:        "access_token_here",
        AuthMethod:   imapclient.AuthXOAUTH2,
        PollTimeout:  2 * time.Minute,
        PollInterval: 5 * time.Second,
    }

    c, err := cfg.LoginXOAUTH2()
    if err != nil {
        log.Fatal(err)
    }
    defer c.Logout()

    body, err := cfg.GetImapMessage(c, "INBOX", "", "")
    if err != nil {
        log.Fatal(err)
    }

    fmt.Println(*body)
}
```

`Token` must be a Microsoft OAuth `access_token` with IMAP permission. It is not the long-lived refresh token that commonly starts with `M.C...`.

## Behavior

- Defaults to `outlook.office365.com:993`.
- Supports direct connection or SOCKS5 proxy.
- Waits for unread matching messages in a loop.
- Marks a matched message as read by default.
- Set `KeepUnread: true` to leave messages unread.
- Set `DeleteAfterRead: true` to mark matched messages deleted.

## Test

```powershell
go test ./...
```
