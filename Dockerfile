# the frontend builder
# cloudreve need node.js 16* to build frontend,
# !!! Doesn't require any cleanup, as multi-stage builds are removed after completion
FROM node:16-alpine as frontend_builder

RUN set -e \
    && apk update \
    && apk add bash wget curl git zip \
    && sh -c "$(curl -sSL https://taskfile.dev/install.sh)" -- -d -b /usr/local/bin
# Maybe copying the current directory is more accurate?
#    && git clone --recurse-submodules https://github.com/cloudreve/Cloudreve.git /cloudreve

COPY . /cloudreve

WORKDIR /cloudreve

RUN task clean-frontend build-frontend


# the backend builder
# cloudreve backend needs golang 1.18* to build
# !!! Doesn't require any cleanup, as multi-stage builds are removed after completion
FROM golang:1.18-alpine as backend_builder

# install dependencies and build tools
RUN set -e \
    && apk update \
    && apk add bash wget curl git build-base \
    && sh -c "$(curl -sSL https://taskfile.dev/install.sh)" -- -d -b /usr/local/bin
# Maybe copying the current directory is more accurate?
#    && git clone --recurse-submodules https://github.com/cloudreve/Cloudreve.git /cloudreve

COPY . /cloudreve

WORKDIR /cloudreve

COPY --from=frontend_builder /cloudreve/assets.zip ./

RUN task clean-backend build-backend "PLATFORM=docker"


# the final builder
FROM alpine:latest

COPY --from=backend_builder /cloudreve/release/cloudreve-docker /usr/local/bin/cloudreve

# !!! For i18n users, do not set timezone
RUN set -e \
    && apk update \
    && apk add bash ca-certificates tzdata \
    && rm -f /var/cache/apk/*

WORKDIR /data

EXPOSE 5212

VOLUME ["/data"]

CMD ["cloudreve"]
