FROM golang:1.24.5 AS build

ARG VERSION
ARG COMMIT_ID

LABEL VERSION=$VERSION
LABEL COMMIT_ID=$COMMIT_ID

WORKDIR /go/src/provisioner
COPY files .

ENV GOFLAGS="-mod=vendor"

RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.PROVISIONER_VERSION=$VERSION -X main.PROVISIONER_COMMIT=$COMMIT_ID" \
    -o /go/bin/provisioner main.go

FROM gcr.io/distroless/static-debian12

WORKDIR /app
COPY --from=build /go/bin/provisioner /app/provisioner

USER nonroot:nonroot
ENTRYPOINT ["/app/provisioner"]