# Matterwick

GitHub Bot that handle the creation of the Cloud test server for Pull Requests in Mattermost Org.

docker run --volume=$(pwd)/config:/matterwick/config --volume=$(pwd)/build/_output/bin/matterwick:/matterwick/matterwick -p 8077:8077 mattermost/matterwick:test