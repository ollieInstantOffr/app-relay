package acme

// "Other (any lego provider)": DNS providers without a dedicated form are
// configured with lego's own environment variables
// (https://go-acme.github.io/lego/dns/). lego reads those variables only while
// a provider is constructed (NewDNSProvider → NewDefaultConfig/env.Get); the
// constructed provider keeps the values in its Config. Relay therefore sets
// the variables for exactly that call and restores the previous environment
// right after.
//
// Concurrency: the process environment is global, so envMu serialises it.
//   - "Other" constructions take the write lock for set → construct → restore,
//     so two of them never see each other's values.
//   - Typed constructions (legoDNSProvider) take the read lock: their
//     NewDefaultConfig also reads lego variables (TTL, timeouts) and must not
//     observe another provider's temporary values.
//   - Nothing else in Relay reads these variables after startup. Keys are
//     restricted to the provider's own namespace (e.g. GANDI_) and *_FILE
//     indirections are rejected, so LEGO_*, proxy and process variables can't
//     be changed even briefly.

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/providers/dns/allinkl"
	"github.com/go-acme/lego/v4/providers/dns/alwaysdata"
	"github.com/go-acme/lego/v4/providers/dns/autodns"
	"github.com/go-acme/lego/v4/providers/dns/bluecat"
	"github.com/go-acme/lego/v4/providers/dns/civo"
	"github.com/go-acme/lego/v4/providers/dns/corenetworks"
	"github.com/go-acme/lego/v4/providers/dns/cpanel"
	"github.com/go-acme/lego/v4/providers/dns/dnsmadeeasy"
	"github.com/go-acme/lego/v4/providers/dns/domeneshop"
	"github.com/go-acme/lego/v4/providers/dns/dreamhost"
	"github.com/go-acme/lego/v4/providers/dns/dyndnsfree"
	"github.com/go-acme/lego/v4/providers/dns/easydns"
	"github.com/go-acme/lego/v4/providers/dns/eurodns"
	"github.com/go-acme/lego/v4/providers/dns/gandi"
	"github.com/go-acme/lego/v4/providers/dns/gcore"
	"github.com/go-acme/lego/v4/providers/dns/glesys"
	"github.com/go-acme/lego/v4/providers/dns/hostingde"
	"github.com/go-acme/lego/v4/providers/dns/hostinger"
	"github.com/go-acme/lego/v4/providers/dns/hosttech"
	"github.com/go-acme/lego/v4/providers/dns/infomaniak"
	"github.com/go-acme/lego/v4/providers/dns/ionoscloud"
	"github.com/go-acme/lego/v4/providers/dns/ipv64"
	"github.com/go-acme/lego/v4/providers/dns/ispconfig"
	"github.com/go-acme/lego/v4/providers/dns/limacity"
	"github.com/go-acme/lego/v4/providers/dns/loopia"
	"github.com/go-acme/lego/v4/providers/dns/luadns"
	"github.com/go-acme/lego/v4/providers/dns/mijnhost"
	"github.com/go-acme/lego/v4/providers/dns/mittwald"
	"github.com/go-acme/lego/v4/providers/dns/mythicbeasts"
	"github.com/go-acme/lego/v4/providers/dns/netlify"
	"github.com/go-acme/lego/v4/providers/dns/nicmanager"
	"github.com/go-acme/lego/v4/providers/dns/njalla"
	"github.com/go-acme/lego/v4/providers/dns/otc"
	"github.com/go-acme/lego/v4/providers/dns/plesk"
	"github.com/go-acme/lego/v4/providers/dns/rackspace"
	"github.com/go-acme/lego/v4/providers/dns/regru"
	"github.com/go-acme/lego/v4/providers/dns/selectel"
	"github.com/go-acme/lego/v4/providers/dns/servercow"
	"github.com/go-acme/lego/v4/providers/dns/simply"
	"github.com/go-acme/lego/v4/providers/dns/spaceship"
	"github.com/go-acme/lego/v4/providers/dns/stackpath"
	"github.com/go-acme/lego/v4/providers/dns/timewebcloud"
	"github.com/go-acme/lego/v4/providers/dns/variomedia"
	"github.com/go-acme/lego/v4/providers/dns/vercel"
	"github.com/go-acme/lego/v4/providers/dns/versio"
	"github.com/go-acme/lego/v4/providers/dns/websupport"
	"github.com/go-acme/lego/v4/providers/dns/wedos"
	"github.com/go-acme/lego/v4/providers/dns/yandex360"
	"github.com/go-acme/lego/v4/providers/dns/zoneedit"
	"github.com/go-acme/lego/v4/providers/dns/zoneee"

	"github.com/instantoffr/relay/internal/model"
)

// envMu guards the process environment for lego provider construction.
var envMu sync.RWMutex

type envProvider struct {
	label  string
	prefix string
	ctor   func() (challenge.Provider, error)
}

func ctor[P challenge.Provider](f func() (P, error)) func() (challenge.Provider, error) {
	return func() (challenge.Provider, error) {
		p, err := f()
		if err != nil {
			return nil, err
		}
		return p, nil
	}
}

// otherProviders are the lego providers offered by "Other". Only providers
// whose dependencies are already in go.sum are listed; providers with a
// dedicated form are not repeated here.
var otherProviders = map[string]envProvider{
	"allinkl":      {"all-inkl", "ALL_INKL_", ctor(allinkl.NewDNSProvider)},
	"alwaysdata":   {"Alwaysdata", "ALWAYSDATA_", ctor(alwaysdata.NewDNSProvider)},
	"autodns":      {"AutoDNS (InterNetX)", "AUTODNS_", ctor(autodns.NewDNSProvider)},
	"bluecat":      {"BlueCat Address Manager", "BLUECAT_", ctor(bluecat.NewDNSProvider)},
	"civo":         {"Civo", "CIVO_", ctor(civo.NewDNSProvider)},
	"corenetworks": {"Core-Networks", "CORENETWORKS_", ctor(corenetworks.NewDNSProvider)},
	"cpanel":       {"cPanel / WHM", "CPANEL_", ctor(cpanel.NewDNSProvider)},
	"dnsmadeeasy":  {"DNS Made Easy", "DNSMADEEASY_", ctor(dnsmadeeasy.NewDNSProvider)},
	"domeneshop":   {"Domeneshop", "DOMENESHOP_", ctor(domeneshop.NewDNSProvider)},
	"dreamhost":    {"DreamHost", "DREAMHOST_", ctor(dreamhost.NewDNSProvider)},
	"dyndnsfree":   {"DynDNS Free", "DYNDNSFREE_", ctor(dyndnsfree.NewDNSProvider)},
	"easydns":      {"easyDNS", "EASYDNS_", ctor(easydns.NewDNSProvider)},
	"eurodns":      {"EuroDNS", "EURODNS_", ctor(eurodns.NewDNSProvider)},
	"gandi":        {"Gandi (legacy API)", "GANDI_", ctor(gandi.NewDNSProvider)},
	"gcore":        {"G-Core", "GCORE_", ctor(gcore.NewDNSProvider)},
	"glesys":       {"GleSYS", "GLESYS_", ctor(glesys.NewDNSProvider)},
	"hostingde":    {"Hosting.de", "HOSTINGDE_", ctor(hostingde.NewDNSProvider)},
	"hostinger":    {"Hostinger", "HOSTINGER_", ctor(hostinger.NewDNSProvider)},
	"hosttech":     {"hosttech", "HOSTTECH_", ctor(hosttech.NewDNSProvider)},
	"infomaniak":   {"Infomaniak", "INFOMANIAK_", ctor(infomaniak.NewDNSProvider)},
	"ionoscloud":   {"IONOS Cloud DNS", "IONOSCLOUD_", ctor(ionoscloud.NewDNSProvider)},
	"ipv64":        {"IPv64", "IPV64_", ctor(ipv64.NewDNSProvider)},
	"ispconfig":    {"ISPConfig 3", "ISPCONFIG_", ctor(ispconfig.NewDNSProvider)},
	"limacity":     {"Lima-City", "LIMACITY_", ctor(limacity.NewDNSProvider)},
	"loopia":       {"Loopia", "LOOPIA_", ctor(loopia.NewDNSProvider)},
	"luadns":       {"LuaDNS", "LUADNS_", ctor(luadns.NewDNSProvider)},
	"mijnhost":     {"mijn.host", "MIJNHOST_", ctor(mijnhost.NewDNSProvider)},
	"mittwald":     {"mittwald", "MITTWALD_", ctor(mittwald.NewDNSProvider)},
	"mythicbeasts": {"Mythic Beasts", "MYTHICBEASTS_", ctor(mythicbeasts.NewDNSProvider)},
	"netlify":      {"Netlify", "NETLIFY_", ctor(netlify.NewDNSProvider)},
	"nicmanager":   {"Nicmanager", "NICMANAGER_", ctor(nicmanager.NewDNSProvider)},
	"njalla":       {"Njalla", "NJALLA_", ctor(njalla.NewDNSProvider)},
	"otc":          {"Open Telekom Cloud", "OTC_", ctor(otc.NewDNSProvider)},
	"plesk":        {"Plesk", "PLESK_", ctor(plesk.NewDNSProvider)},
	"rackspace":    {"Rackspace", "RACKSPACE_", ctor(rackspace.NewDNSProvider)},
	"regru":        {"reg.ru", "REGRU_", ctor(regru.NewDNSProvider)},
	"selectel":     {"Selectel", "SELECTEL_", ctor(selectel.NewDNSProvider)},
	"servercow":    {"Servercow", "SERVERCOW_", ctor(servercow.NewDNSProvider)},
	"simply":       {"Simply.com", "SIMPLY_", ctor(simply.NewDNSProvider)},
	"spaceship":    {"Spaceship", "SPACESHIP_", ctor(spaceship.NewDNSProvider)},
	"stackpath":    {"StackPath", "STACKPATH_", ctor(stackpath.NewDNSProvider)},
	"timewebcloud": {"Timeweb Cloud", "TIMEWEBCLOUD_", ctor(timewebcloud.NewDNSProvider)},
	"variomedia":   {"Variomedia", "VARIOMEDIA_", ctor(variomedia.NewDNSProvider)},
	"vercel":       {"Vercel", "VERCEL_", ctor(vercel.NewDNSProvider)},
	"versio":       {"Versio", "VERSIO_", ctor(versio.NewDNSProvider)},
	"websupport":   {"Websupport", "WEBSUPPORT_", ctor(websupport.NewDNSProvider)},
	"wedos":        {"WEDOS", "WEDOS_", ctor(wedos.NewDNSProvider)},
	"yandex360":    {"Yandex 360", "YANDEX360_", ctor(yandex360.NewDNSProvider)},
	"zoneedit":     {"ZoneEdit", "ZONEEDIT_", ctor(zoneedit.NewDNSProvider)},
	"zoneee":       {"Zone.ee", "ZONEEE_", ctor(zoneee.NewDNSProvider)},
}

func init() {
	opts := make([]model.DNSFieldOption, 0, len(otherProviders))
	for code, p := range otherProviders {
		opts = append(opts, model.DNSFieldOption{Value: code, Label: p.label, EnvPrefix: p.prefix, DocsURL: "https://go-acme.github.io/lego/dns/" + code + "/"})
	}
	sort.Slice(opts, func(i, j int) bool { return opts[i].Label < opts[j].Label })
	for i := range model.DNSProviderTypes {
		pt := &model.DNSProviderTypes[i]
		if pt.Type != model.DNSProviderOther {
			continue
		}
		for j := range pt.Fields {
			if pt.Fields[j].Key == model.DNSOtherProviderKey {
				pt.Fields[j].Options = opts
			}
		}
	}
}

// withEnv sets vars, runs fn and restores the previous values (or unsets
// variables that did not exist). Callers must hold envMu for writing.
func withEnv(vars map[string]string, fn func()) {
	type saved struct {
		val string
		had bool
	}
	prev := make(map[string]saved, len(vars))
	for k, v := range vars {
		old, had := os.LookupEnv(k)
		prev[k] = saved{old, had}
		_ = os.Setenv(k, v)
	}
	defer func() {
		for k, s := range prev {
			if s.had {
				_ = os.Setenv(k, s.val)
			} else {
				_ = os.Unsetenv(k)
			}
		}
	}()
	fn()
}

// otherDNSProvider constructs a lego provider from environment-variable
// credentials (type "other").
func otherDNSProvider(creds map[string]string) (challenge.Provider, time.Duration, error) {
	code := creds[model.DNSOtherProviderKey]
	entry, ok := otherProviders[code]
	if !ok {
		return nil, 0, fmt.Errorf("unsupported lego provider %q", code)
	}
	vars := map[string]string{}
	for k, v := range creds {
		if k == model.DNSOtherProviderKey {
			continue
		}
		if err := model.ValidLegoEnvKey(k, entry.prefix); err != nil {
			return nil, 0, fmt.Errorf("%s: %w", k, err)
		}
		vars[k] = v
	}
	var prov challenge.Provider
	var err error
	envMu.Lock()
	withEnv(vars, func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%s: %v", code, r)
			}
		}()
		prov, err = entry.ctor()
	})
	envMu.Unlock()
	if err != nil {
		return nil, 0, err
	}
	timeout := 2 * time.Minute
	if t, ok := prov.(challenge.ProviderTimeout); ok {
		if d, _ := t.Timeout(); d > 0 {
			timeout = d
		}
	}
	return prov, timeout, nil
}
