CHART_DIR := charts/entroq
REGO_SRC  := pkg/authz/opadata/conf
CRD_SRC   := cmd/eqk8s/config/crd/bases

##@ Helm

REGO_SOURCES := \
	$(REGO_SRC)/core/entroq/authz/core-entroq-authz.rego \
	$(REGO_SRC)/core/entroq/jwt/core-entroq-jwt.rego \
	$(REGO_SRC)/core/entroq/namespaces/core-entroq-namespaces.rego \
	$(REGO_SRC)/core/entroq/queues/core-entroq-queues.rego \
	$(REGO_SRC)/providers/k8s/permissions/k8s-entroq-permissions.rego \
	$(REGO_SRC)/providers/k8s/user/k8s-entroq-user.rego
CRD_SOURCES  := $(wildcard $(CRD_SRC)/*.yaml)

# Stamp file: touched after a successful sync.
# Make re-syncs only when a source file is newer than the stamp.
HELM_SYNC_STAMP := $(CHART_DIR)/files/.sync-stamp

$(HELM_SYNC_STAMP): $(REGO_SOURCES) $(CRD_SOURCES)
	@mkdir -p $(CHART_DIR)/files/rego $(CHART_DIR)/crds
	cp $(REGO_SOURCES) $(CHART_DIR)/files/rego/
	cp $(CRD_SOURCES)  $(CHART_DIR)/crds/
	@touch $@
	@echo "helm-sync: chart files up to date."

.PHONY: helm-sync
helm-sync: $(HELM_SYNC_STAMP) ## Sync Rego files and CRDs into the chart (incremental).

.PHONY: helm-lint
helm-lint: helm-sync ## Lint the chart.
	helm lint $(CHART_DIR)

.PHONY: helm-package
helm-package: helm-sync helm-lint ## Package the chart into a .tgz.
	helm package $(CHART_DIR) --destination charts/

.PHONY: helm-template
helm-template: helm-sync ## Render the chart to stdout (useful for reviewing output).
	helm template entroq $(CHART_DIR)

.PHONY: help
help: ## Show this help.
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
