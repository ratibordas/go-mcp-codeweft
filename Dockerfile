FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/codeweft ./cmd/codeweft

FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=build /out/codeweft /usr/local/bin/codeweft
ENTRYPOINT ["codeweft"]
CMD ["help"]
