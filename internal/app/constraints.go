package app

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/labstack/onebox/internal/imageref"
)

// The grammars and the sentences explaining their violation live together.
//
// This is the part of a schema language worth keeping: a constraint separated
// from its error message drifts from it, and a constraint separated from its
// documentation drifts from that too. One table, one place to read what a field
// may contain and what an author is told when it does not.
//
// Every pattern here is anchored. An unanchored pattern accepts a value with
// anything appended, which for a field that reaches a generated file is the
// whole problem.

type grammar struct {
	name    string
	pattern *regexp.Regexp
	// means is what the field may contain, phrased for the person who wrote
	// the value rather than for whoever wrote the pattern.
	means string
}

var (
	// An identifier names something Onebox derives further names from, so it
	// is bounded to what is safe in a container name, a volume name, a network
	// name and a DNS label at once.
	gIdent = grammar{"identifier", regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`),
		"lower-case letters, digits and hyphens, starting with a letter, at most 40 characters"}

	gDur = grammar{"duration", regexp.MustCompile(`^(([0-9]+([.][0-9]+)?(ns|us|µs|ms|s|m|h))+|[0-9]+d)$`),
		"a duration such as 30s, 5m, 1h30m or 14d"}

	gCron = grammar{"cron expression", regexp.MustCompile(`^[-0-9*/,A-Za-z ]+$`),
		"five cron fields"}

	// An IANA zone name. Bounded because it is written verbatim into a
	// scheduling unit on the target, where a newline appends whatever follows
	// it and that runs as root.
	gTZ = grammar{"timezone", regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+)*$`),
		"an IANA zone name such as UTC or Europe/Berlin"}

	gSignal = grammar{"signal", regexp.MustCompile(`^[A-Z][A-Z0-9]*$`),
		"a signal name such as TERM or QUIT"}

	gSize = grammar{"size", regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(B|KB|MB|GB|TB)$`),
		"a size such as 512MB or 1.5GB"}

	gCpus = grammar{"cpu count", regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`),
		"a number of CPUs such as 0.5 or 2"}

	// Repository-relative. Absolute is refused lexically; escaping the
	// repository is a semantic check the loader performs after resolution,
	// because `a/../b` is legal and resolves inside.
	gRepoPath = grammar{"repository path", regexp.MustCompile("^[^/\\x00-\\x1f'\"$`\\\\][^\\x00-\\x1f'\"$`\\\\]*$"),
		"a path inside the repository, with no control character or shell metacharacter"}

	gAbsPath = grammar{"absolute path", regexp.MustCompile("^/[^\\x00-\\x1f'\"$`\\\\]*$"),
		"an absolute path with no control character or shell metacharacter"}

	gURLPath = grammar{"url path", regexp.MustCompile("^/[^\\x00-\\x1f'\"$` \\\\]*$"),
		"a path beginning with /"}

	// A registry reference. The registry library owns this grammar; keeping its
	// pattern here also makes the generated JSON schema agree with runtime
	// validation.
	gImageRef = grammar{"image reference", imageref.Pattern,
		"a registry reference such as nginx:1.27 or ghcr.io/acme/app@sha256:…"}

	gEnvName = grammar{"environment variable", regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`),
		"a variable name of letters, digits and underscores, not starting with a digit"}

	gRegistryHost = grammar{"registry server", regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*(:[0-9]{1,5})?(/[A-Za-z0-9._/-]*)?$`),
		"a host with an optional port and path, such as ghcr.io or registry.example.com:5000"}

	gRegistryUser = grammar{"registry username", regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]*$`),
		"a username of letters, digits and . _ @ + -"}

	gCalVer = grammar{"version", regexp.MustCompile(`^v[0-9]{4}\.(0[1-9]|1[0-2])\.[1-9][0-9]*$`),
		"a CalVer release such as v2026.08.1"}

	gHTTPURL = grammar{"url", regexp.MustCompile(`^https?://`),
		"an http or https URL"}

	// A referenced Compose service name belongs to the file the author wrote,
	// not to this contract: Compose accepts underscores and Onebox has no
	// standing to refuse a service someone else already named.
	gComposeService = grammar{"compose service name", regexp.MustCompile(`^[a-zA-Z0-9._-]+$`),
		"a Compose service name"}

	// file#service, where the file part is repository-relative.
	gComposeRef = grammar{"compose reference", regexp.MustCompile("^[^/#][^#]*#[a-zA-Z0-9._-]+$"),
		"a reference of the form path/to/compose.yaml#service"}

	gPlanSchema = grammar{"plan schema", regexp.MustCompile(`^onebox\.run/executable-deploy-plan/v[1-9][0-9]*((alpha|beta)[1-9][0-9]*)?$`),
		"a plan schema identity such as onebox.run/executable-deploy-plan/v1alpha2"}

	gFailureDomain = grammar{"failure-domain identity", regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`),
		"a stable identifier of letters, digits, dots, colons, slashes, underscores and hyphens"}

	gBucket = grammar{"bucket name", regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`),
		"a lower-case S3-compatible bucket name between 3 and 63 characters"}

	gObjectPrefix = grammar{"object prefix", regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$`),
		"a relative object prefix with no empty leading component or shell metacharacter"}

	gS3Region = grammar{"S3 region", regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`),
		"a lower-case S3-compatible region of letters, digits and hyphens"}

	gProtectionOwner = grammar{"protection owner", regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@:/-]{0,127}$`),
		"a stable operator or provider identity of letters, digits, dots, @, colons, slashes, underscores and hyphens"}
)

// enums are the closed value sets, each with the field it belongs to.
var (
	eImagePull     = []string{"always", "missing", "never"}
	eRouteProtocol = []string{"http", "tcp"}
	eRouteScheme   = []string{"http", "https", "h2c"}
	eRouteTLS      = []string{"terminate", "passthrough", "none"}
	eMountMode     = []string{"rw", "ro"}
	ePersistence   = []string{"durable", "ephemeral", "external"}
	eNeedCondition = []string{"started", "healthy", "completed"}
	ePortProtocol  = []string{"tcp", "udp"}
	eStrategy      = []string{"rolling", "recreate"}
	eJobWhen       = []string{"pre_release", "post_release", "manual"}
	// The seams the engine actually invokes. An unlisted name loads fine and
	// never runs, so the set is closed: a hook that silently does not fire is
	// worse than one refused at load.
	eHookSeam        = []string{"bootstrap", "pre_release", "post_release", "post_deploy"}
	eDataEffect      = []string{"none", "migration", "destructive", "unknown"}
	eMigrationPolicy = []string{"manual", "auto", "expand-only"}
	eNotifyFormat    = []string{"text", "json"}
	eProxyKind       = []string{"traefik-docker", "none"}
	// One provider, because one is implemented. The withdrawn `secrets` block
	// accepted `age` and nothing ever decrypted it — every path shells out to
	// `sops -d` — so carrying it forward meant `provider: age` validated,
	// rendered a filename, and staged nothing, failing on the target with a
	// name the author never wrote. Accepting a value no code can honour is not
	// generosity.
	eSecretProvider    = []string{"sops"}
	eRole              = []string{RoleApplication, RoleWorker, RoleDaemon, RoleJob}
	eConnectionPart    = []string{"url", "host", "port", "user", "password", "database"}
	eBackupTargetKind  = []string{"s3-compatible"}
	eBackupTLS         = []string{"required", "insecure"}
	eRecoveryKind      = []string{"snapshot", "pitr", "cold"}
	eEncryptionMode    = []string{"client-side", "archive-password", "server-side-sse"}
	eExternalProbeKind = []string{"driver-health"}
)

// reservedAppNames are the identities the host layout already uses. An
// application taking one of them would derive names that collide with the
// proxy's or the host namespace's, and the collision would appear as a
// container that vanishes rather than as an error.
var reservedAppNames = []string{"ob", "proxy", "_host"}

// checkAppName is the identifier grammar plus the reservations.
func checkAppName(name string) error {
	if err := gIdent.check("app", name); err != nil {
		return err
	}
	if strings.HasPrefix(name, "ob-") {
		return errf("project_invalid", "app", "",
			"%q begins with \"ob-\", which names host-scoped resources Onebox owns", name)
	}
	for _, reserved := range reservedAppNames {
		if name == reserved {
			return errf("project_invalid", "app", "",
				"%q is reserved for the host layout", name)
		}
	}
	return nil
}

// check reports a value that does not match its grammar, naming the path, what
// was written, and what the field may contain.
func (g grammar) check(path, value string) error {
	if g.pattern.MatchString(value) {
		return nil
	}
	return errf("project_invalid", path, "",
		"%q is not %s: expected %s", value, g.name, g.means)
}

// checkOptional skips an unset value. An absent field is the schema's business,
// not a grammar's.
func (g grammar) checkOptional(path, value string) error {
	if value == "" {
		return nil
	}
	return g.check(path, value)
}

func checkImageRef(path, value string) error {
	if err := imageref.Validate(value); err == nil {
		return nil
	}
	return errf("project_invalid", path, "",
		"%q is not %s: expected %s", value, gImageRef.name, gImageRef.means)
}

func checkOptionalImageRef(path, value string) error {
	if value == "" {
		return nil
	}
	return checkImageRef(path, value)
}

func checkEnum(path, value string, allowed []string) error {
	if value == "" {
		return nil
	}
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return errf("project_invalid", path, "",
		"%q is not one of %s", value, strings.Join(quoteAll(allowed), ", "))
}

func checkPositive(path string, value int) error {
	if value <= 0 {
		return errf("project_invalid", path, "", "must be a positive whole number, got %d", value)
	}
	return nil
}

func checkPort(path string, value int) error {
	if value < 1 || value > 65535 {
		return errf("project_invalid", path, "", "%d is not a port between 1 and 65535", value)
	}
	return nil
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}
