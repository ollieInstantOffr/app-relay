package auth

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,31}$`)

func usernameError(u string) string {
	switch {
	case u == "":
		return "Username is required"
	case !usernameRe.MatchString(u):
		return "2–32 characters: letters, digits, dot, dash and underscore"
	}
	return ""
}

// passwordError returns a user-facing message when pw violates the policy.
func passwordError(pw, username string) string {
	switch {
	case utf8.RuneCountInString(pw) < MinPasswordLength:
		return fmt.Sprintf("At least %d characters", MinPasswordLength)
	case len(pw) > 72:
		return "At most 72 bytes"
	case username != "" && strings.EqualFold(pw, username):
		return "Must be different from the username"
	}
	for _, r := range pw {
		if unicode.IsControl(r) {
			return "Control characters aren't allowed"
		}
	}
	return ""
}

// HashPassword returns a bcrypt hash (cost 12).
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	return string(h), err
}

func checkPassword(hash, pw string) bool {
	if hash == "" || len(pw) > 72 {
		burnPasswordCheck(pw)
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

var (
	dummyOnce sync.Once
	dummyHash []byte
)

// burnPasswordCheck spends the same time as a real bcrypt comparison so that
// unknown usernames can't be distinguished by response time.
func burnPasswordCheck(pw string) {
	dummyOnce.Do(func() {
		dummyHash, _ = bcrypt.GenerateFromPassword([]byte("relay-timing-equaliser"), bcryptCost)
	})
	if len(pw) > 72 {
		pw = pw[:72]
	}
	_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(pw))
}

// GeneratePassword returns a memorable random password such as
// "brisk-otter-cedar-4471": one adjective, words-1 nouns and four digits.
func GeneratePassword(words int) string {
	if words < 2 {
		words = 2
	}
	parts := make([]string, 0, words+1)
	parts = append(parts, passwordAdjectives[randIndex(len(passwordAdjectives))])
	for i := 1; i < words; i++ {
		parts = append(parts, passwordNouns[randIndex(len(passwordNouns))])
	}
	parts = append(parts, fmt.Sprintf("%04d", randIndex(10000)))
	return strings.Join(parts, "-")
}

func randIndex(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return int(v.Int64())
}

var passwordAdjectives = []string{
	"agile", "alpine", "amber", "azure", "bold", "brave", "bright", "brisk", "calm", "cheery", "chilly", "clever",
	"coral", "cosmic", "cozy", "crisp", "daring", "dapper", "dreamy", "dusty", "eager", "early", "fair", "fancy",
	"fleet", "fresh", "fuzzy", "gentle", "glad", "golden", "grand", "gusty", "happy", "hardy", "hazel", "humble",
	"icy", "ivory", "jade", "jazzy", "jolly", "keen", "kind", "lanky", "lemon", "lively", "loyal", "lucky",
	"lunar", "magic", "maple", "mellow", "merry", "mild", "misty", "modest", "mossy", "neat", "nifty", "nimble",
	"noble", "oaken", "olive", "pearl", "peppy", "perky", "plucky", "polar", "polite", "prime", "proud", "quick",
	"quiet", "rapid", "ready", "rosy", "royal", "ruby", "rustic", "sandy", "sharp", "shiny", "silent", "silky",
	"silver", "sleek", "smart", "snappy", "snowy", "solar", "solid", "spicy", "spry", "stable", "steady", "stormy",
	"sturdy", "sunny", "sweet", "swift", "tender", "tidy", "tiny", "true", "velvet", "vivid", "warm", "windy",
	"wise", "witty", "woolly", "young", "zesty", "breezy", "cobalt", "copper", "dewy", "frosty", "grassy", "hushed",
	"juicy", "leafy", "mighty", "nutty", "rainy", "rocky", "shady", "stony",
}

var passwordNouns = []string{
	"acorn", "anchor", "aspen", "badger", "beaver", "birch", "bison", "breeze", "brook", "canyon", "cedar", "cliff",
	"clover", "cobra", "comet", "condor", "coyote", "crane", "cricket", "delta", "dingo", "dolphin", "dune", "eagle",
	"ember", "falcon", "fern", "ferret", "finch", "fjord", "forest", "fox", "gecko", "glacier", "goose", "grove",
	"harbor", "heron", "hornet", "husky", "ibis", "island", "jackal", "koala", "lagoon", "lark", "lemur", "lion",
	"llama", "lynx", "magpie", "marmot", "marten", "meadow", "mesa", "mink", "moose", "moth", "newt", "ocelot",
	"orbit", "orca", "osprey", "otter", "owl", "panda", "parrot", "pebble", "pelican", "penguin", "prairie", "puffin",
	"quail", "quartz", "rabbit", "raven", "reef", "ridge", "river", "robin", "salmon", "seal", "shark", "sparrow",
	"squid", "stork", "summit", "swan", "tapir", "thicket", "tiger", "toucan", "trout", "tundra", "turtle", "valley",
	"viper", "walrus", "weasel", "whale", "willow", "wolf", "wombat", "yak", "zebra", "badlands", "basin", "bay",
	"bluff", "cavern", "cove", "crater", "creek", "geyser", "gorge", "hollow", "knoll", "lake", "marsh", "oasis",
	"pond", "rapids", "spring", "grotto",
}
