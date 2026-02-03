FROM registry.k8shell.io/docker/library/golang:1.24.5 AS build

ARG VERSION
ARG COMMIT_ID

LABEL VERSION=$VERSION
LABEL COMMIT_ID=$COMMIT_ID

WORKDIR /go/src/provisioner
COPY go.mod go.sum ./
COPY vendor ./vendor
COPY main.go ./
COPY internal ./internal
COPY pkg ./pkg

ENV GOFLAGS="-mod=vendor"

RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.PROVISIONER_VERSION=$VERSION -X main.PROVISIONER_COMMIT=$COMMIT_ID" \
    -o /go/bin/provisioner main.go

FROM registry.k8shell.io/docker/library/alpine:3.21.3

RUN mkdir /app
COPY --from=build /go/bin/provisioner /app

WORKDIR /app
