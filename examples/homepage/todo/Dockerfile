FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go page.html ./
RUN CGO_ENABLED=0 go build -trimpath -o /todo .

FROM alpine:3.22
COPY --from=build /todo /usr/local/bin/todo
USER nobody
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/todo"]
