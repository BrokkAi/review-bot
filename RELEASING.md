# Releasing review-bot

Run `make check build`, inspect `git diff`, and commit the complete change.
The shared ACP dependency must be a published version, with no local replace
or workspace override needed to build. Keep `go.mod` and `go.sum` committed.

Push master and a new semver tag such as `v0.3.1`. That is the only release
trigger needed. `publish-packages.yml` runs Linux/macOS CI through the reusable
`release.yml`, builds and publishes the native GitHub assets, then builds/tests
and uploads all five npm packages at the exact same tag and commit. A failed
native release prevents npm publication. Follow the **Publish packages** run for
the complete result. npm's trusted publisher remains `publish-packages.yml` in
the `packages-publish` environment. The environment accepts only `v*` tags.
npm publishing uses OIDC; no `NPM_TOKEN` secret is required.

The installer expects `brokk-review-bot-VERSION-OS-ARCH.tar.gz`, containing `brv`,
and `checksums.txt`. Supported targets are Linux/macOS, amd64/arm64.
`python3 scripts/package_release.py v0.1.1` builds these archives locally.
The tag is also the Go module release. No separate Go upload is needed.
Python publication is not configured.

The package job validates native checksums, package contents, local installs and
existing-version integrity before uploading. Platform packages are submitted
before the launcher. Upload errors fail the job; successful npm uploads do not
wait for public version indexes or run immediate public-install smoke tests.

For a partial npm failure, rerun failed jobs or dispatch `publish-packages.yml`
from the exact existing tag with its tag input and `publish=true`. Use
`publish=false` for validation without uploads. Matching existing package bytes
are retained; conflicting versions stop publication. The explicit
`python3 scripts/package_registry.py verify dist/packages` command remains
available for later public-integrity checks after registry propagation.

If publication fails after draft creation, inspect the draft and uploaded assets.
Complete or replace that draft explicitly; do not move published version tags.

## License validation

Before committing release preparation, run `python3 scripts/licenses.py`.
For dependency or Go version changes, follow [licenses/README.md](licenses/README.md)
to review the policy and regenerate notices. Native packaging repeats this
check and includes the exact project license, notice, and dependency report.
Every npm package retains these files from the verified native assets. The
package smoke test inspects their bytes as well as exercising installation.

## Bootstrapping new package names

npm requires each package to exist before a trusted publisher can be added.
Complete the following before the first stable tag when creating a new package
namespace. Existing packages do not need this repeated.

The npm namespace contains `@brokkai/review-bot` and four platform packages:
`@brokkai/review-bot-linux-x64`, `@brokkai/review-bot-linux-arm64`,
`@brokkai/review-bot-darwin-x64`, and `@brokkai/review-bot-darwin-arm64`.
Create the GitHub `packages-publish` environment with a deployment tag policy
matching `v*`. From a clean committed checkout, build and validate a complete
release candidate, such as `v0.1.0-rc.1`. Sign in with `npm login` and publish
those exact artifacts using `scripts/package_registry.py publish`; prereleases
use npm's `next` dist-tag. No bootstrap credential needs to be stored in GitHub.

With npm 11.15.0 or newer and an account with package write access and 2FA,
configure each package:

```sh
for package in review-bot review-bot-linux-x64 review-bot-linux-arm64 review-bot-darwin-x64 review-bot-darwin-arm64; do
  npm trust github "@brokkai/$package" --repo BrokkAi/review-bot \
    --file publish-packages.yml --env packages-publish --allow-publish --yes
  sleep 2
done
```

Complete npm's interactive 2FA challenge when requested, then verify all five
configurations with `npm trust list`. See the
[npm trust documentation](https://docs.npmjs.com/cli/v11/commands/npm-trust/).
Push the stable tag only after trust is configured. Confirm that GitHub Actions
publishes that new version using OIDC and that all public package integrity
values match the validated artifacts.

For local validation, start from a clean committed checkout, use Go from
`go.mod` and Node.js 24, and run:

```sh
make check build
python3 -m unittest discover -s scripts -p '*_test.py'
node --test --test-isolation=none npm/brv.test.cjs
python3 scripts/smoke_installers.py
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
```

The smoke script builds all native targets and all five npm packages, checks
legal payloads, installs local packages without contacting the npm registry,
and exercises `brv --help`. It does not publish anything. No Python package
is configured for review-bot.
