FROM golang:1.24.5 AS build

ARG VERSION
ARG COMMIT_ID

LABEL VERSION=$VERSION
LABEL COMMIT_ID=$COMMIT_ID

WORKDIR /go/src/provisioner
COPY files .

# need to use 1.16.4 due to regression bug in 1.16.5
# https://github.com/swaggo/swag/issues/2042
RUN go install github.com/swaggo/swag/cmd/swag@v1.16.4
RUN /go/bin/swag init --generalInfo internal/server/restapi.go --output docs

ENV GOFLAGS="-mod=vendor"

RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.PROVISIONER_VERSION=$VERSION -X main.PROVISIONER_COMMIT=$COMMIT_ID" \
    -o /go/bin/provisioner main.go

FROM alpine:3.21.3

RUN mkdir /app
COPY --from=build /go/bin/provisioner /app
COPY --from=build /go/src/provisioner/docs /app/docs

WORKDIR /app
