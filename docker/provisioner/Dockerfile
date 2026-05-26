# syntax=docker/dockerfile:1
# Copyright 2025 the k8Shell authors.
# SPDX-License-Identifier: AGPL-3.0-or-later

# -- Build stage ---------------------------------------------------------------
FROM registry.k8shell.io/docker/library/golang:1.24.5 AS build-debug

ARG VERSION
ARG COMMIT_ID

LABEL VERSION=$VERSION
LABEL COMMIT_ID=$COMMIT_ID

WORKDIR /go/src/provisioner

COPY go.mod go.sum ./
COPY vendor ./vendor

COPY main.go ./
COPY internal ./internal

ENV GOFLAGS="-mod=vendor"

RUN CGO_ENABLED=0 go build -gcflags="all=-N -l" \
    -ldflags="-X main.PROVISIONER_VERSION=$VERSION -X main.PROVISIONER_COMMIT=$COMMIT_ID" \
    -o /go/bin/provisioner main.go

# -- Build stage (release) -----------------------------------------------------
FROM registry.k8shell.io/docker/library/golang:1.24.5 AS build-release

ARG VERSION
ARG COMMIT_ID

WORKDIR /go/src/provisioner

COPY go.mod go.sum ./
COPY vendor ./vendor

COPY main.go ./
COPY internal ./internal

ENV GOFLAGS="-mod=vendor"

RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X main.PROVISIONER_VERSION=$VERSION -X main.PROVISIONER_COMMIT=$COMMIT_ID" \
    -o /go/bin/provisioner main.go

# -- Runtime stage (Alpine) ----------------------------------------------------
FROM registry.k8shell.io/docker/library/alpine:3.21.3 AS alpine

RUN mkdir /app
COPY --from=build-debug /go/bin/provisioner /app/provisioner
WORKDIR /app
ENTRYPOINT ["sh", "-c", "while true; do /app/provisioner \"$@\"; done", "--"]

# -- Runtime stage (distroless) ------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS distroless

COPY --from=build-release --chown=nonroot:nonroot /go/bin/provisioner /app/provisioner

USER nonroot:nonroot
WORKDIR /app
ENTRYPOINT ["/app/provisioner"]
