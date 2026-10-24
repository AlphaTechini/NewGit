# Plain Dockerfile syntax (no BuildKit-only flags) so it also builds on
# Cloud Build's classic docker builder via `gcloud run deploy --source .`.

FROM golang:1.26-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# Templates, CSS, JS and fonts are embedded with go:embed, so the binary is
# the only artifact the runtime needs.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/newgit ./cmd/newgit

FROM alpine:3.24
# git does all repository work (NewGit needs 2.38+); busybox sh runs the
# pre-receive and proc-receive hook scripts NewGit writes at startup.
RUN apk add --no-cache git \
    && addgroup -S -g 10001 newgit \
    && adduser -S -D -u 10001 -G newgit -h /home/newgit newgit \
    && mkdir /data \
    && chown newgit:newgit /data
COPY --from=build /out/newgit /usr/local/bin/newgit
USER 10001:10001
ENV NEWGIT_ADDR=:8080 \
    NEWGIT_DATA=/data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/newgit"]
