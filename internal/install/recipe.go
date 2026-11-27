package install

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/values"
)

// This file embeds the PROVEN in-cluster data-service recipe as Go values. It is
// grounded in a proven in-cluster llamacloud install and the
// proven local recipe, so the CLI owns the exact stand-up that was previously
// performed out-of-band. The chart versions, images, and creds here MUST
// stay in lock-step with that proven recipe.
//
// Credentials are FIXED (not randomly generated) on purpose: a re-run must
// upgrade the DB releases in place (idempotent), which only works if the creds
// are stable across runs. Random per-run creds would leave the already-running
// database on its old password. Generating+persisting creds as k8s secrets is a
// later refinement; the fixed demo creds are what the proven recipe uses.
//
// Postgres carries EXPLICIT resource requests/limits (not resourcesPreset:
// "micro" like redis): under the combined llamacloud + in-cluster Temporal load
// the "micro" preset's ~192Mi is too small and the postgres pod is OOMKilled.
// redis does not OOM, so it keeps the "micro" preset.
//
// When a storageClass is supplied (clusters with NO default StorageClass), it is
// injected into each release's global block so the DB PVCs bind
// instead of hanging Pending forever (which would time out helm --wait). An empty
// storageClass injects no key, so the cluster default is used.

// In-cluster data-service credentials. These match the proven in-cluster values + the service
// endpoints wired into the llamacloud values. They are throwaway in-cluster DB
// creds, not customer secrets.
const (
	inClusterPGDatabase = "llamacloud"
	inClusterPGUser     = "llamacloud"
	inClusterPGPassword = "llamacloud"

	inClusterRedisPassword = "password"
)

// In-cluster data-service endpoints. Service names are produced deterministically
// by the bitnami charts installed under the fixed release names below (redis
// standalone exposes redis-master; the others match their release name).
const (
	inClusterPGHost    = "postgresql"
	inClusterPGPort    = "5432"
	inClusterRedisHost = "redis-master"
	inClusterRedisPort = "6379"
)

// karpenterDoNotDisrupt returns the pod-annotation map that opts a pod out of
// Karpenter voluntary disruption (consolidation + drift). It returns a FRESH map
// on every call: these maps are merged into release values that Helm coalescing
// may mutate in place, so a shared instance could be aliased across releases.
//
// Why the in-cluster data services need it: postgres, redis, and the Temporal
// server are singletons backed by an RWO EBS PVC. When Karpenter consolidates an
// "Underutilized" node MID-INSTALL it evicts the pod and reschedules it onto a
// new node; the EBS volume then stalls on a multi-attach delay detaching from the
// old node, postgres restarts into WAL crash-recovery, and the Temporal history
// service crash-loops ("sql schema version compatibility check failed ... no
// usable database connection found"). Its CrashLoopBackOff outlives `helm --wait`
// and the install fails with "context deadline exceeded". Pinning these pods off
// consolidation keeps the datastore stable so the rollout converges. The
// annotation is inert on clusters without Karpenter.
func karpenterDoNotDisrupt() map[string]any {
	return map[string]any{"karpenter.sh/do-not-disrupt": "true"}
}

// globalBlock builds the per-release global values map. It always carries the
// allowInsecureImages security flag every bitnamilegacy/xavidop image needs after
// the Bitnami image freeze (the charts refuse non-canonical images without it).
// When storageClass != "" it also sets global.storageClass so the DB PVCs bind on
// clusters with no default StorageClass; an empty storageClass omits the key and
// the cluster default is used.
func globalBlock(storageClass string) map[string]any {
	g := map[string]any{
		"security": map[string]any{"allowInsecureImages": true},
	}
	if storageClass != "" {
		g["storageClass"] = storageClass
	}
	return g
}

// inClusterDBReleases returns the pinned recipe for the two sibling data-service
// helm releases installed into the target namespace. The order matters only for
// readability; each is installed independently. postgres is listed first because
// the in-cluster Temporal subchart (temporal.deploy=true, part of the llamacloud
// chart) reuses it.
func inClusterDBReleases(storageClass string) []DBRelease {
	return []DBRelease{
		{
			Release: "postgresql",
			Chart:   "oci://registry-1.docker.io/bitnamicharts/postgresql",
			Version: "18.7.3",
			Values: map[string]any{
				"image":  map[string]any{"registry": "docker.io", "repository": "bitnamilegacy/postgresql"},
				"auth":   map[string]any{"enabled": true, "database": inClusterPGDatabase, "username": inClusterPGUser, "password": inClusterPGPassword},
				"global": globalBlock(storageClass),
				"primary": map[string]any{
					// Keep postgres off Karpenter consolidation: a mid-install
					// eviction bounces the EBS PVC (multi-attach delay + WAL
					// recovery) and crash-loops the Temporal history service that
					// reuses this datastore, timing out helm --wait.
					"podAnnotations": karpenterDoNotDisrupt(),
					// Explicit resources (NOT resourcesPreset "micro"): the preset's
					// ~192Mi OOMKills postgres under the llamacloud + Temporal load.
					"resources": map[string]any{
						"requests": map[string]any{"cpu": "250m", "memory": "512Mi"},
						"limits":   map[string]any{"cpu": "1", "memory": "1Gi"},
					},
					// Grant CREATEDB so the in-cluster Temporal subchart
					// (createDatabase: true) can create its temporal /
					// temporal_visibility databases on first boot.
					"initdb": map[string]any{"scripts": map[string]any{
						"grant_createdb.sql": "ALTER USER " + inClusterPGUser + " CREATEDB;",
					}},
				},
			},
		},
		{
			Release: "redis",
			Chart:   "oci://registry-1.docker.io/bitnamicharts/redis",
			Version: "27.0.4",
			Values: map[string]any{
				"image":        map[string]any{"registry": "docker.io", "repository": "bitnamilegacy/redis"},
				"auth":         map[string]any{"password": inClusterRedisPassword},
				"architecture": "standalone",
				"global":       globalBlock(storageClass),
				// standalone redis runs a single master pod backed by a PVC — keep
				// it off Karpenter consolidation for the same reason as postgres.
				"master":          map[string]any{"podAnnotations": karpenterDoNotDisrupt()},
				"resourcesPreset": "micro",
			},
		},
	}
}

// wireInClusterDBValues writes the deterministic in-cluster data-service
// endpoints + creds into the llamacloud values so the chart points at the
// releases inClusterDBReleases installs. It is pure (no cluster access): the
// endpoints are fixed service names, so they can be wired at collection time
// before the release is actually stood up. Temporal is set to deploy in-cluster
// (a subchart that reuses the postgres above), NOT a separate release.
func wireInClusterDBValues(v valueEditor) {
	// postgres (always required)
	v.Set("postgresql.host", inClusterPGHost)
	v.Set("postgresql.port", inClusterPGPort)
	v.Set("postgresql.database", inClusterPGDatabase)
	v.Set("postgresql.username", inClusterPGUser)
	v.Set("postgresql.password", inClusterPGPassword)

	// redis (always required); in-cluster redis is non-TLS -> plain redis scheme
	v.Set("redis.host", inClusterRedisHost)
	v.Set("redis.port", inClusterRedisPort)
	v.Set("redis.password", inClusterRedisPassword)
	v.Set("redis.db", 0)
	v.Set("redis.scheme", "redis")

	// The data services the CLI no longer provisions are explicitly disabled,
	// along with the v1 parse worker that consumes one of them.
	disableRemovedServices(v)

	// temporal: deploy the in-cluster subchart (reuses the in-cluster postgres above).
	wireInClusterTemporal(v, inClusterPGHost, inClusterPGPort, inClusterPGUser, inClusterPGPassword)
}

// wireInClusterTemporal deploys the Temporal in-cluster subchart and points its
// default + visibility SQL datastores at the SAME PostgreSQL the app uses —
// passed in as pgHost/pgPort/pgUser/pgPassword. The in-cluster Temporal subchart
// has no database of its own; it reuses whatever postgres the install resolves
// to (in-cluster service, an existing external endpoint, or a managed RDS
// instance), so its datastore connection MUST mirror postgresql.* rather than
// hardcode the in-cluster service name — otherwise a managed/external postgres
// install points Temporal at a "postgresql" host that does not resolve and the
// schema job's create-default-store init container CrashLoopBackOffs on DNS
// ("lookup postgresql ... no such host").
//
// Setting only temporal.deploy=true is NOT enough: the temporal-subchart renders
// fail with "Please specify connectAddr for default store" unless the datastore
// connection is configured. createDatabase lets Temporal create its own temporal
// / temporal_visibility databases on that server (the in-cluster postgres user is
// granted CREATEDB by inClusterDBReleases' initdb script; the RDS master user has
// it by default).
//
// It ALSO decides the datastore's SQL TLS block — see wireTemporalDatastoreTLS.
// A managed/external postgres gets TLS on, the in-cluster service does not.
//
// pgHost may be empty when postgres is provisioned managed (its endpoint is
// unknown until Ensure runs after approval): connectAddr is then left empty (and
// no TLS block is written) and runManagedProvisioning re-invokes this helper once
// the RDS endpoint is merged, which is when both land.
func wireInClusterTemporal(v valueEditor, pgHost, pgPort, pgUser, pgPassword string) {
	v.Set("temporal.disabled", false)
	v.Set("temporal.deploy", true)

	// Keep the Temporal server pods (frontend/history/matching/worker) off
	// Karpenter consolidation. server.podAnnotations is merged onto every server
	// pod by the chart's temporal.resourceAnnotations helper. Without this, a
	// consolidation eviction of the history pod mid-install — on top of any
	// postgres bounce — restarts its schema/DB-connection check and its
	// CrashLoopBackOff outlives helm --wait ("context deadline exceeded").
	v.Set("temporal-subchart.server.podAnnotations", karpenterDoNotDisrupt())

	// Empty host (managed postgres, endpoint not yet provisioned) => leave
	// connectAddr empty; the managed-aware Temporal guard passes it up front and
	// runManagedProvisioning re-wires it after the endpoint merges.
	addr := ""
	if pgHost != "" {
		addr = pgHost + ":" + pgPort
	}
	for _, ds := range []struct{ store, db string }{
		{"default", "temporal"},
		{"visibility", "temporal_visibility"},
	} {
		p := temporalDatastoreKeyPrefix(ds.store)
		v.Set(p+"connectAddr", addr)
		v.Set(p+"connectProtocol", "tcp")
		v.Set(p+"driverName", "postgres12")
		v.Set(p+"pluginName", "postgres12")
		v.Set(p+"databaseName", ds.db)
		v.Set(p+"user", pgUser)
		v.Set(p+"password", pgPassword)
		v.Set(p+"createDatabase", true)
		v.Set(p+"manageSchema", true)
		wireTemporalDatastoreTLS(v, p, isExternalPGHost(pgHost))
	}
	v.Set("temporal-subchart.server.config.persistence.defaultStore", "default")
	v.Set("temporal-subchart.server.config.persistence.visibilityStore", "visibility")
	v.Set("temporal-subchart.server.config.namespaces.create", true)
}

// temporalDatastoreKeyPrefix returns the dotted values prefix of one Temporal
// subchart SQL datastore ("default" / "visibility"), trailing dot included. Every
// producer AND reader of those keys goes through here so a chart-side rename is a
// one-line change rather than a scavenger hunt through string literals.
func temporalDatastoreKeyPrefix(store string) string {
	return "temporal-subchart.server.config.persistence.datastores." + store + ".sql."
}

// temporalDatastoreTLSEnabledKey is the dotted values path of the DEFAULT
// datastore's TLS toggle — the key an operator overrides in their -f values file,
// and the one the plan render and the guard read back, so the reported TLS state
// cannot drift from what the wiring writes.
var temporalDatastoreTLSEnabledKey = temporalDatastoreKeyPrefix("default") + "tls.enabled"

// isExternalPGHost reports whether a resolved postgres host is something OUTSIDE
// the cluster — a managed endpoint (RDS / Cloud SQL / Flexible Server) or an
// operator-supplied external server — as opposed to the in-cluster bitnami
// service this installer stands up. An empty host means "not resolved yet"
// (managed postgres before Ensure) and is NOT external: the caller re-runs the
// wiring once the endpoint exists.
//
// It mirrors hasExternalEndpoint's rule for the postgresql prefix, applied to an
// already-resolved host string.
func isExternalPGHost(host string) bool {
	host = strings.TrimSpace(host)
	return host != "" && host != inClusterPGHost
}

// wireTemporalDatastoreTLS writes the Temporal subchart's SQL TLS block for ONE
// datastore when the postgres it points at is external.
//
// WHY THIS EXISTS. A managed postgres enforces TLS: an RDS instance created with
// the default parameter group has rds.force_ssl=1, so an unencrypted connection is
// rejected by pg_hba before authentication. The subchart's schema-setup Job is a
// helm PRE-INSTALL HOOK that gates every other workload in the release, and its
// create-default-store init container connects with no TLS unless this block is
// set — so the whole install dies at Init:CrashLoopBackOff with
//
//	pq: no pg_hba.conf entry for host <pod-ip>, user llamacloud, database postgres, no encryption
//
// and the rollout can never converge.
//
// THE KEYS ARE VERIFIED AGAINST THE PINNED SUBCHART, not upstream docs: temporal
// 1.2.0's templates/_admintools-env.yaml reads
// <datastore>.sql.tls.{enabled,caFile,certFile,keyFile,serverName,enableHostVerification}
// and emits SQL_TLS from `enabled` plus, when the key is PRESENT,
// SQL_TLS_DISABLE_HOST_VERIFICATION from `not enableHostVerification`. The key is
// the POSITIVE enableHostVerification — there is no disableHostVerification key.
// The same block is passed through verbatim into the server's own config by
// templates/server-configmap.yaml.
//
// WHY HOST VERIFICATION IS OFF. Verifying an RDS endpoint requires the AWS RDS CA
// bundle in the pod's trust store, and neither this installer nor the chart puts
// it there — verification would fail the connection outright. enabled=true with
// verification off is "encrypt, do not validate the server certificate", which is
// what satisfies force_ssl. Mounting the RDS CA and turning verification back on
// (tls.caFile + tls.enableHostVerification=true) is a documented hardening
// follow-up, not a blocker: the subchart supports both keys.
//
// The operator's own choice ALWAYS wins: if the -f values file already carries
// tls.enabled for this datastore (true OR false), nothing is written. That is the
// override for a plaintext-only external postgres, and it is why this takes a
// valueEditor rather than a valueSetter.
func wireTemporalDatastoreTLS(v valueEditor, prefix string, external bool) {
	if _, ok := v.Get(prefix + "tls.enabled"); ok {
		return
	}
	if !external {
		// In-cluster postgres: the bitnami release serves plaintext only, so an
		// enabled TLS block would break a path that works today. Write nothing.
		return
	}
	v.Set(prefix+"tls.enabled", true)
	v.Set(prefix+"tls.enableHostVerification", false)
}

// buildInClusterDBPlan wires the in-cluster data-service endpoints and creds into
// v and returns the matching ProvisionPlan (in-cluster DB releases, no buckets).
// It is the DB-only subset of the wizard's minimal path (applyMinimalDefaults
// additionally wires buckets): the non-interactive --provision-in-cluster entry
// point provisions only the DBs, since a scripted caller names its own object
// storage in the seed values. Returning the plan (rather than mutating one) makes
// it directly assertable in a test, mirroring RunWizard.
//
// # Precedence: a supplied endpoint WINS
//
// --provision-in-cluster is SCOPED to the services whose endpoint the values do
// not already supply. A service whose host came from --from-terraform (or a plain
// -f seed) keeps that endpoint and no in-cluster release is recorded for it; the
// flag then only covers what is left. This is a design decision, not a fallback:
// the flag used to overwrite BOTH services unconditionally, so
// `install --from-terraform outputs.json --provision-in-cluster` silently
// discarded the terraform-supplied RDS/ElastiCache endpoints and ran
// the platform on in-cluster databases instead.
//
// The combination is NOT an error, because it is exactly what an operator who
// needs in-cluster Temporal on top of terraform-supplied databases has to pass:
// with terraform-supplied DBs, --provision-in-cluster means "deploy Temporal
// in-cluster against the postgres you supplied". Every kept endpoint is announced
// on out, so the scoping is stated rather than silent.
//
// Temporal is wired LAST, against whichever postgres won — supplied or in-cluster
// — and picks up the TLS block wireInClusterTemporal decides from that host.
func buildInClusterDBPlan(v *values.Values, storageClass string, out io.Writer) *ProvisionPlan {
	if out == nil {
		out = io.Discard
	}
	plan := &ProvisionPlan{}
	for _, svc := range []struct{ kind, prefix string }{
		{"postgres", "postgresql"},
		{"redis", "redis"},
	} {
		if hasExternalEndpoint(v, svc.prefix) {
			_, _ = fmt.Fprintf(out, "  - %s: keeping the endpoint your values supply (%s) — --provision-in-cluster covers only services without one.\n",
				svc.prefix, getString(v, svc.prefix+".host"))
			continue
		}
		release, ok := dbReleaseFor(svc.kind, storageClass)
		if !ok {
			// Unreachable: both kinds here have a recipe. Kept as a guard so a future
			// kind added to the loop without a recipe cannot silently wire endpoints
			// for a release that is never installed.
			continue
		}
		wireInClusterService(v, svc.kind)
		plan.InClusterDBs = append(plan.InClusterDBs, release)
		_, _ = fmt.Fprintf(out, "  - %s: standing up in-cluster (%s %s).\n", svc.prefix, release.Chart, release.Version)
	}

	// The data services the CLI no longer provisions are explicitly disabled, along
	// with the v1 parse worker that consumes one of them.
	disableRemovedServices(v)

	// Temporal deploys as a subchart of the main chart (no separate release), wired
	// to WHICHEVER postgres won above.
	wireInClusterTemporal(v,
		getString(v, "postgresql.host"),
		getString(v, "postgresql.port"),
		getString(v, "postgresql.username"),
		getString(v, "postgresql.password"))
	_, _ = fmt.Fprintf(out, "  - temporal: deploying the in-cluster subchart against %s.\n",
		getString(v, temporalDatastoreKeyPrefix("default")+"connectAddr"))
	return plan
}

// wireInClusterService wires the deterministic in-cluster endpoint + creds for a
// SINGLE data service (used by the customize path when only some services are
// stood up in-cluster rather than the whole default set). It does not touch the
// service's enable toggle or temporal — dataServicesStage owns those.
func wireInClusterService(v valueSetter, kind string) {
	switch kind {
	case "postgres":
		v.Set("postgresql.host", inClusterPGHost)
		v.Set("postgresql.port", inClusterPGPort)
		v.Set("postgresql.database", inClusterPGDatabase)
		v.Set("postgresql.username", inClusterPGUser)
		v.Set("postgresql.password", inClusterPGPassword)
	case "redis":
		v.Set("redis.host", inClusterRedisHost)
		v.Set("redis.port", inClusterRedisPort)
		v.Set("redis.password", inClusterRedisPassword)
		v.Set("redis.db", 0)
		v.Set("redis.scheme", "redis")
	}
}

// dbReleaseFor returns the single DBRelease recipe for one service kind, used by
// the customize path when only some services are stood up in-cluster. The
// storageClass (empty = cluster default) is threaded into the release's global
// block, mirroring inClusterDBReleases.
func dbReleaseFor(kind, storageClass string) (DBRelease, bool) {
	name := kind
	if kind == "postgres" {
		name = "postgresql"
	}
	for _, r := range inClusterDBReleases(storageClass) {
		if r.Release == name {
			return r, true
		}
	}
	return DBRelease{}, false
}

// removedDataServices are the data services the CLI no longer provisions or
// wires, with the platform consequence of each being disabled. LlamaCloud v2
// drops these, but charts that still ship them default their enable toggle ON
// and then require an endpoint — so disableRemovedServices explicitly turns
// them off to keep the assembled values renderable (and keep the app from trying
// to reach a service the operator did not configure). The plan render surfaces
// the disabled set so the operator approves it knowingly (see renderPlan).
var removedDataServices = []struct{ key, consequence string }{
	{"mongodb", "features that require it — pipelines, v1 jobs, mongodb index export — return errors"},
	{"rabbitmq", "the jobs-worker (AMQP consumer) is not deployed; v1 parse and pipeline ingestion return errors"},
	{"qdrant", "the Qdrant data-sink integration is unavailable (chart default is already off)"},
}

// v1ParseWorkerKey is the chart values prefix of the v1 parse worker — the
// `llamacloud-parse` Deployment. Verified against the published chart's
// values.schema.json: a top-level `llamaParse` object carrying `replicas`
// (type integer|null, minimum 0). There is NO llamaParse.enabled flag, so
// replicas: 0 is the only lever the schema offers.
const v1ParseWorkerKey = "llamaParse"

// v1ParseWorkerReplicasKey is the dotted path the installer writes to scale the
// v1 parse worker down.
const v1ParseWorkerReplicasKey = v1ParseWorkerKey + ".replicas"

// zeroV1ParseWorker scales the chart's v1 parse worker (llamacloud-parse) to
// zero replicas.
//
// WHY. The v1 parse worker's ONLY work channel is the RabbitMQ queue: it has no
// HTTP work intake, and its readiness probe never passes without an AMQP
// connection. Because this installer hard-disables rabbitmq (see
// removedDataServices), the pod can never become ready — so an install left at
// the chart's default replica count stalls its rollout wait at 19-of-20 pods and
// the release lands status=failed even though the platform itself is fully up.
// Confirmed: scaling llamaParse to 0 takes every remaining pod
// to 1/1 Ready and /api/health to 200.
//
// Parsing is NOT affected. The live v2 parse path runs in the separate
// llamacloud-temporal-parse deployment (values key temporalWorkloads.llamaParse),
// which this does not touch.
//
// It leaves the value ALONE when the operator's own values file already asks for
// a nonzero replica count. That is explicit intent and the installer does not
// silently overrule it; the values preflight (preflight.InstallValuesChecks)
// WARNs about the combination instead, and renderPlan reports the kept count.
func zeroV1ParseWorker(v valueEditor) {
	if n, ok := values.Int(v.Get(v1ParseWorkerReplicasKey)); ok && n != 0 {
		return
	}
	v.Set(v1ParseWorkerReplicasKey, 0)
}

// disableRemovedServices turns OFF every removed data service (see
// removedDataServices for the set and the consequences) and scales the v1 parse
// worker to zero, because that worker consumes only the rabbitmq queue this
// disables (see zeroV1ParseWorker). Every path that assembles install values —
// the wizard's customize path, the in-cluster recipe, and the non-interactive
// seed path — goes through here so the two can never drift apart.
func disableRemovedServices(v valueEditor) {
	for _, svc := range removedDataServices {
		v.Set(svc.key+".enabled", false)
	}
	zeroV1ParseWorker(v)
}

// dropRemovedServicesTheChartLacks deletes values the CLI writes for options
// that newer charts removed, when the target chart's values.schema.json would
// reject them, and returns the deleted dotted paths, sorted. A key is rejected
// when its parent object in the schema does not list it and sets
// additionalProperties to false.
//
// The paths: each removed data service and the v1 parse worker (charts from
// 0.11.0 ship neither rabbitmq nor llamaParse), and `temporal.disabled` when it
// is false (charts from 0.10.0 dropped the flag; false was its default, so
// dropping it changes nothing). A true `temporal.disabled` stays, so validation
// still refuses an install that asked for Temporal off. With no schema, or one
// that accepts the key, nothing is deleted.
func dropRemovedServicesTheChartLacks(vals map[string]any, schemaJSON []byte) []string {
	if len(schemaJSON) == 0 {
		return nil
	}
	var root schemaNode
	if err := json.Unmarshal(schemaJSON, &root); err != nil {
		return nil
	}
	paths := []string{v1ParseWorkerKey}
	for _, svc := range removedDataServices {
		paths = append(paths, svc.key)
	}
	if b, ok := values.FromMap(vals).Get("temporal.disabled"); ok && b == false {
		paths = append(paths, "temporal.disabled")
	}
	var dropped []string
	for _, path := range paths {
		if deleteIfRejected(vals, &root, strings.Split(path, ".")) {
			dropped = append(dropped, path)
		}
	}
	sort.Strings(dropped)
	return dropped
}

// schemaNode is the slice of a JSON schema object that
// dropRemovedServicesTheChartLacks reads.
type schemaNode struct {
	AdditionalProperties additionalProperties  `json:"additionalProperties"`
	Properties           map[string]schemaNode `json:"properties"`
}

// additionalProperties holds a schema's additionalProperties keyword, which is
// either a boolean or a schema object (e.g. a map of typed overrides). Only the
// literal false forbids unknown keys; true, an object or absence allows them.
// Typing it as *bool made json.Unmarshal fail on the first object-valued one
// (chart 0.13.0 has several), and a failed parse dropped nothing.
type additionalProperties struct{ forbidden bool }

func (a *additionalProperties) UnmarshalJSON(b []byte) error {
	a.forbidden = strings.TrimSpace(string(b)) == "false"
	return nil
}

// deleteIfRejected walks segs through vals and node together and deletes the
// last segment when node's object at that level lists it nowhere and forbids
// additional properties. It reports whether it deleted anything.
func deleteIfRejected(vals map[string]any, node *schemaNode, segs []string) bool {
	key := segs[0]
	child, defined := node.Properties[key]
	if len(segs) == 1 {
		if _, set := vals[key]; !set || defined || !node.AdditionalProperties.forbidden {
			return false
		}
		delete(vals, key)
		return true
	}
	next, ok := vals[key].(map[string]any)
	if !ok || !defined {
		return false
	}
	return deleteIfRejected(next, &child, segs[1:])
}

// valueSetter is the subset of *values.Values the pure wiring helpers need. It
// keeps recipe.go decoupled from the concrete values type for testing.
type valueSetter interface {
	Set(dotted string, val any)
}

// valueEditor is valueSetter plus the read side, for the helpers that must
// read-modify-write — i.e. those that have to see an operator-supplied value
// before deciding whether to overwrite it.
type valueEditor interface {
	valueSetter
	Get(dotted string) (any, bool)
}
