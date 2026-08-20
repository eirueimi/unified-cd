# Complete Example

## Complete Example

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: ci-pipeline
spec:
  params:
    inputs:
      - name: image
        type: string
        required: true
      - name: tag
        type: string
        default: latest
      - name: deploy_env
        type: string
        default: staging
    outputs:
      - name: image_ref
        type: string
  agentSelector:
    - kind:linux
    - env:ci
  concurrency:
    mutex: "deploy-{{ .Params.deploy_env }}"
  timeoutMinutes: 60

  steps:
    - parallel:
        - name: lint
          run: golangci-lint run ./...
          timeoutMinutes: 10

        - name: test
          run: go test -race ./...
          timeoutMinutes: 20

    - name: build
      run: |
        go build -o bin/server ./cmd/server
        echo "Build successful"
      outputs:
        binary_path: bin/server

    - name: upload-binary
      uploadArtifact:
        name: server-binary
        path: bin/server

    - name: build-image
      env:
        REGISTRY_PASS: "{{ secrets.REGISTRY_PASS }}"
      run: |
        echo "$REGISTRY_PASS" | docker login registry.example.com --password-stdin -u ci
        docker build -t {{ .Params.image }}:{{ .Params.tag }} .
        docker push {{ .Params.image }}:{{ .Params.tag }}
      outputs:
        image_ref: "{{ .Params.image }}:{{ .Params.tag }}"

    - name: deploy-staging
      if: 'params.deploy_env == "staging"'
      run: |
        ./deploy.sh --env staging --image {{ .Steps.build-image.Outputs.image_ref }}

    - name: deploy-production
      if: 'params.deploy_env == "production"'
      run: |
        ./deploy.sh --env production --image {{ .Steps.build-image.Outputs.image_ref }}
```
