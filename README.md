# Matterwick

GitHub Bot that handle the creation of the Cloud test server for Pull Requests in Mattermost Org.

### Deployment Configuration

In order to utilize Kubernetes Spinwick creation, matteriwck must be able to access the following 2 **environment** variables:
```
AWS_ACCESS_KEY_ID=
AWS_SECRET_ACCESS_KEY=
```
You can find how to generate these by following the documentation [https://aws.amazon.com/premiumsupport/knowledge-center/create-access-key/](here)

### Local Development

To run Matterwick locally:
Copy config/config-matterwick.default.json to config-matterwick.json: `cp config/config-matterwick.default.json config/config-matterwick.json`
Populate the values in config-matterwick.json

Run:
```
make build-image # Build the matterwick image (also builds the go code)
make build # (optional) can be used later to rebuild the code without rebuilding the entire image
docker run --volume=$(pwd)/config:/matterwick/config --volume=$(pwd)/build/_output/bin/matterwick:/matterwick/matterwick --volume=$(echo $HOME)/.kube:/.kube --volume=$(pwd)/templates:/matterwick/templates -p 8077:8077 --env-file=.env mattermost/matterwick:test
```
The above assumes you have a .kube/config.json file that is already authenticated with a Kubernetes cluster.
For use with ngrok, you can input `ngrok http 8077`. You can then point your github webhook at the ngrok URL.
To rebuild the go code run `make build`. 
You do not need to rebuild the docker image unless you make changes to the Dockerfile. You must restart your docker container after a `make build` in order to see changes



### E2E repo profiles

Every E2E flow uses the repo's profile (`server/e2e_profiles.go`). Adding a repo means adding a profile:

| Repo | Servers | Workflow | CMT |
|---|---|---|---|
| `*desktop*` | `linux`, `macos`, `windows` | `e2e-functional.yml` | yes |
| `*mobile*` | `android-site-1/2`, `ios-site-1/2`, `site-3` | `e2e-detox-pr.yml` | yes |
| `mattermost-mobile-oxide` (exact name, checked first) | `android-site-1..3`, `ios-site-1..3` | `e2e-matterwick.yml` | no |

Same rules for every repo:
- **PR labels** (`E2E/Run`, `E2E/Run-iOS`, `E2E/Run-Android`): servers `<kind>-pr-<N>-*`. A run gets fresh servers when nothing else is using the PR's servers, otherwise it reuses them. Each workflow cancels its own superseded runs. PR close and `E2E/Reset-Servers` delete `<kind>-pr-<N>-*` and stop any provisioning. A PR's servers are removed once no run on it is queued or running and the last one was dispatched more than `E2EPRIdleTimeoutMinutes` (60) ago — never while a run is using them.
- **Pushes to the default branch** (`E2EAutoTriggerOnMaster`): fresh `<kind>-push-*` servers, dispatched on that branch with the pushed SHA. A push superseded while its servers came up is skipped. Its servers are destroyed when that run completes, or by the stale scan after `E2EInstanceMaxAge`.
- **Provisioning**: at most `E2EMaxConcurrentProvisions` (64) servers are created at once; more wait their turn. A failed server is deleted and replaced after a backoff; the set fails only when one server gives up, and the error names it.

Oxide is off unless `E2EOxideEnabled` is `true`; its servers are cleaned up either way.
