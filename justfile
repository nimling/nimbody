install:
    sh scripts/install.sh

deploy bump='patch':
    sbump {{bump}} --env APP_VERSION --push-version --auto --workflow

build:
    go build ./...

tidy:
    go mod tidy

test name="":
    go test ./test/... {{ if name != "" { "-run " + quote(name) } else { "" } }}
