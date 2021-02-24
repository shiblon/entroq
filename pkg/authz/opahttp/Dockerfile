FROM openpolicyagent/opa:0.26.0

# Core files always present.
COPY /config/rego/core/ ./pkg/authz/opadata/core/

# Example policy and username binding can be overwritten with volume mounts.
# Also copy example data with very permissive wildcard role. OVerride with volume mount.
COPY /config/rego/policy/ ./pkg/authz/opadata/example/*.rego
COPY /config/data/ ./pkg/authz/opadata/example/data.json

VOLUME /config/policy
VOLUME /config/data

EXPOSE 8181

CMD [
  "--server",
  "--watch",
  "--addr", ":8181",
  "--config-file", "/config/config.yaml",
  "--bundle", "/config/rego:/config/policy:/config/data"
]
