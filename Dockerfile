# Build the matterwick
# Build image: golang:1.19.5
ARG DOCKER_BUILD_IMAGE=golang@1.22-alpine
# Base image: alpine 3.16.3
ARG DOCKER_BASE_IMAGE=alpine@sha256:3d426b0bfc361d6e8303f51459f17782b219dece42a1c7fe463b6014b189c86d

FROM ${DOCKER_BUILD_IMAGE} AS build
WORKDIR /matterwick/
COPY . /matterwick/
RUN make build

# Final Image
FROM ${DOCKER_BASE_IMAGE}

LABEL name="MatterWick" \
  maintainer="cloud-team@mattermost.com" \
  distribution-scope="public" \
  architecture="x86_64" \
  url="https://mattermost.com"

ENV MATTERWICK=/matterwick/matterwick \
    USER_UID=10001 \
    USER_NAME=matterwick

WORKDIR /matterwick/

RUN  apk update && apk add ca-certificates

COPY --from=build /matterwick/build/_output/bin/matterwick /matterwick/matterwick
COPY --from=build /matterwick/build/bin /usr/local/bin
COPY --from=build /matterwick/templates /matterwick/templates

RUN  /usr/local/bin/user_setup

EXPOSE 8077

ENTRYPOINT ["/usr/local/bin/entrypoint"]

USER ${USER_UID}
