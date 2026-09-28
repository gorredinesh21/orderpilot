.PHONY: run test vet build docker deploy clean

run: ## run locally (LLM live if HF_TOKEN is exported, else fallback planner)
	go run . 

test: ## full test suite
	go test ./...

vet: ## static analysis
	go vet ./...

build: ## static linux binary
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/orderpilot .

docker: ## build the distroless image
	docker build -t orderpilot .

deploy: ## deploy to Cloud Run (requires gcloud auth)
	gcloud run deploy orderpilot \
		--project personal-project-dg21 --region us-central1 \
		--source . --allow-unauthenticated \
		--set-env-vars HF_TOKEN=$${HF_TOKEN},HF_LLM_MODEL=meta-llama/Llama-3.1-8B-Instruct \
		--min-instances=0 --max-instances=2 --memory=512Mi --cpu=1 \
		--timeout=120

clean:
	rm -rf bin
