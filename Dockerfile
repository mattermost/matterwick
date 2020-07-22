# Build the matterwick
ARG DOCKER_BUILD_IMAGE=golang:1.14.2
ARG DOCKER_BASE_IMAGE=alpine:3.11.3

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

RUN apk add --no-cache python py-pip

RUN pip install awscli

COPY --from=build /matterwick/build/_output/bin/matterwick /matterwick/matterwick
COPY --from=build /matterwick/build/bin /usr/local/bin

RUN  /usr/local/bin/user_setup

EXPOSE 8077

ENTRYPOINT ["/usr/local/bin/entrypoint"]

USER ${USER_UID}
