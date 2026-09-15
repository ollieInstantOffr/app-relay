// Package deps pins third-party modules used across feature slices so
// go.mod stays stable while slices are developed in parallel.
package deps

import (
	_ "filippo.io/age"
	_ "github.com/docker/docker/client"
	_ "github.com/go-acme/lego/v4/lego"
	_ "github.com/go-acme/lego/v4/providers/dns/cloudflare"
	_ "github.com/go-acme/lego/v4/providers/dns/digitalocean"
	_ "github.com/go-acme/lego/v4/providers/dns/duckdns"
	_ "github.com/go-acme/lego/v4/providers/dns/hetzner"
	_ "github.com/go-acme/lego/v4/providers/dns/route53"
	_ "github.com/go-acme/lego/v4/providers/http/webroot"
	_ "github.com/go-webauthn/webauthn/webauthn"
	_ "github.com/modelcontextprotocol/go-sdk/mcp"
	_ "github.com/pquerna/otp/totp"
	_ "golang.org/x/crypto/bcrypt"
)
