# Commercial runtime enforcement

API and mixed Worker consumers bind to `settlement` in compiled code. Approved
Compose components receive independent `runtime/license-<service>.env` files,
read-only platform public keys and separate durable signed-state volumes.
Credentials and signing private keys must not enter images, logs or shared
application environment files. Legacy compatibility remains disabled-by-default
enforcement until controlled enrollment; enabled incomplete configuration fails.

API historical query/download routes and historical report generation are
explicitly classified, not inferred from HTTP method. Unknown entries and all
new business mutations require entitlement; existing authentication, CSRF and
permission checks are preserved.

The mixed Worker retains existing audit/notification delivery and historical
report export. New dunning scans, tax reconciliation and business outbox
destinations check entitlement before claiming/executing work and again before
external business calls. License pauses do not mark delivery successful or spend
business failure attempts; leased work becomes eligible again after its lease.

Signed snapshots remain locally usable during loss of the platform connection,
only until their verified absolute deadline. Restart does not reset expiry.
Privileged deployment configuration is trusted; this is not protection against
root altering binaries or deleting configuration.

Verify `sh scripts/license-core-sync.sh --check` before build. Release approval
requires actual immutable image configuration digests and a nonempty reviewed
`APP_VERSION`, not mutable tags or container labels supplied by an operator.
The Docker runtime declares protocol `com.basic-platform.license.protocol=1`.
