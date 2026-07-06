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

##@ Schema

# The Go schema is canonical; the Python client ships a byte-identical copy so
# pip-only users can init a database without a Go toolchain. TestSchemaFilesInSync
# (pkg/backend/eqpg) fails if they drift or their version constants disagree.
.PHONY: schema-sync
schema-sync: ## Copy the canonical Postgres schema into the Python client.
	cp pkg/backend/eqpg/schema.sql clients/py/src/entroq/experimental/pg/schema.sql
	@echo "schema-sync: Python schema updated from canonical."
	@echo "If the schema version changed, also align SCHEMA_VERSION in clients/py/src/entroq/experimental/pg/__init__.py."

##@ Help

.PHONY: help
help: ## Show this help.
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
