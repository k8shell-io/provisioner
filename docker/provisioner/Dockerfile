FROM fitcr.ksi.in.fit.cvut.cz/docker/library/golang:1.24.5 AS build

ARG VERSION
ARG COMMIT_ID

LABEL VERSION=$VERSION
LABEL COMMIT_ID=$COMMIT_ID

WORKDIR /go/src/provisioner
COPY files .

ENV GOFLAGS="-mod=vendor"

RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.PROVISIONER_VERSION=$VERSION -X main.PROVISIONER_COMMIT=$COMMIT_ID" \
    -o /go/bin/provisioner main.go

FROM fitcr.ksi.in.fit.cvut.cz/docker/library/alpine:3.21.3

RUN mkdir /app
COPY --from=build /go/bin/provisioner /app

WORKDIR /app
