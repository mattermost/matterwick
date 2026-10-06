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

PR labels (`E2E/Run`, `E2E/Run-iOS`, `E2E/Run-Android`, `E2E/Reset-Servers`) provision Mattermost Cloud test servers and dispatch the repo's test workflow:

| Repo | Servers per PR | Dispatched workflow | Push / CMT |
|---|---|---|---|
| `*desktop*` | 3 (`linux`, `macos`, `windows`) | `e2e-functional.yml` | yes |
| `*mobile*` | 5 (`android-site-1/2`, `ios-site-1/2`, `site-3`) | `e2e-detox-pr.yml` | yes |
| `mattermost-mobile-oxide` (exact name, checked before `*mobile*`) | 6 (`android-site-1..3`, `ios-site-1..3`), DNS prefix `oxide-` | `e2e-matterwick.yml` | no |

The Oxide profile is off unless `E2EOxideEnabled` is `true`; while off, Oxide labels are ignored. PR close and `E2E/Reset-Servers` only ever delete `oxide-pr-<N>-*` for Oxide.
