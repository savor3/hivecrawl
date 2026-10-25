FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /hivecrawl ./cmd/hivecrawl && mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /hivecrawl /hivecrawl
# Holds the node identity key; owned by the distroless nonroot user.
COPY --from=build --chown=65532:65532 /data /data
ENV HC_DATA_DIR=/data
EXPOSE 3002
ENTRYPOINT ["/hivecrawl"]
