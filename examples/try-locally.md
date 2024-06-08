# Trying out Vault Secrets Reloader locally

To gain experience in this tool, and to get familiar with the potential of the Bank-Vaults ecosystem, in this guidle we will:

- start a [kind](https://kind.sigs.k8s.io/) cluster
- install the [Vault Operator](https://github.com/bank-vaults/vault-operator), [Secrets Webhook](https://github.com/bank-vaults/secrets-webhook) and Vault Secrets Reloader all configured to work together nicely
- start a Vault instance
- deploy some workloads
- try out some scenarios with the Reloader

You only need Docker and the [Vault CLI](https://developer.hashicorp.com/vault/tutorials/getting-started/getting-started-install#install-vault) to be installed!

## 1. Prepare the environment

Clone the [repo](https://github.com/bank-vaults/vault-secrets-reloader) and `cd` into it. With only a few `make` commands, you will have a kind cluster running with the Bank-Vaults ecosystem, including the Reloader:

```bash
# install dependencies
make deps

# start a kind cluster with Bank-Vaults operator, a Vault instance and Secrets Webhook
make up-kind

# build the Vault Secrets Reloader image
make container-image

# deploy Vault Secrets Reloader
make deploy-kind
```

The last command will install the Reloader Helm chart with the following configuration:

```bash
helm upgrade --install vault-secrets-reloader deploy/charts/vault-secrets-reloader \
    --set image.tag=dev \
    --set collectorSyncPeriod=30s \
    --set reloaderRunPeriod=1m \
    --set env.VAULT_ROLE=reloader \
    --set env.VAULT_ADDR=https://vault.default.svc.cluster.local:8200 \
    --set env.VAULT_TLS_SECRET=vault-tls \
    --set env.VAULT_TLS_SECRET_NS=bank-vaults-infra \
    --namespace bank-vaults-infra
```

Two important set of configurations are being set here:

- Time periods for the `collector` and `reloader` - these are set to be very frequent here for the sake of not waiting too long during testing the tool. In real world scenarios they can be set to a higher value (depending on your requirements) not to spam the Kubernetes and Vault API with too many requests.
- Environment variables necessary for the Reloader to create a client for communicating with our Vault instance.

To trigger a new rollout for the affected workloads, you must be able to change secrets in Vault. If you followed the previous steps, you can export these environmental variables and port-forward the Vault pod to be manageable with the Vault CLI (that needs to be [installed](https://developer.hashicorp.com/vault/tutorials/getting-started/getting-started-install#install-vault) separately):

```bash
export VAULT_TOKEN=$(kubectl get secrets vault-unseal-keys -o jsonpath={.data.vault-root} | base64 --decode)

kubectl get secret vault-tls -o jsonpath="{.data.ca\.crt}" | base64 --decode > $PWD/vault-ca.crt
export VAULT_CACERT=$PWD/vault-ca.crt

export VAULT_ADDR=https://127.0.0.1:8200

kubectl port-forward service/vault 8200 &
```

## 2. Deploy workloads

Now that we have the Bank-Vaults ecosystem running in our kind cluster, we can deploy some workloads:

```bash
# deploy some workloads
kubectl apply -f e2e/deploy/workloads
```

Looking at the manifest of one of the deployments, the only difference from one that is prepared to work with the Bank-Vaults Webhook with all the annotations starting with `secrets-webhook.security.bank-vaults.io` and the env values starting with `vault:` is the presence of the new `alpha.vault.security.banzaicloud.io/reload-on-secret-change: "true"` annotation telling the Reloader to collect secrets and reload it if necessary.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: reloader-test-deployment-to-be-reloaded
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: reloader-test-deployment-to-be-reloaded
  template:
    metadata:
      labels:
        app.kubernetes.io/name: reloader-test-deployment-to-be-reloaded
      annotations:
        secrets-webhook.security.bank-vaults.io/vault-addr: "https://vault:8200"
        secrets-webhook.security.bank-vaults.io/vault-tls-secret: vault-tls
        alpha.vault.security.banzaicloud.io/reload-on-secret-change: "true"
    spec:
      initContainers:
        - name: init-ubuntu
          image: ubuntu
          command: ["sh", "-c", "echo $AWS_SECRET_ACCESS_KEY && echo $MYSQL_PASSWORD && echo initContainers ready"]
          env:
            - name: AWS_SECRET_ACCESS_KEY
              value: vault:secret/data/accounts/aws#AWS_SECRET_ACCESS_KEY
            - name: MYSQL_PASSWORD
              value: vault:secret/data/mysql#${.MYSQL_PASSWORD}
          resources:
            limits:
              memory: "128Mi"
              cpu: "100m"
      containers:
        - name: alpine
          image: alpine
          command:
            - "sh"
            - "-c"
            - "echo $AWS_SECRET_ACCESS_KEY && echo $MYSQL_PASSWORD && echo going to sleep... && sleep 10000"
          env:
            - name: AWS_SECRET_ACCESS_KEY
              value: vault:secret/data/accounts/aws#AWS_SECRET_ACCESS_KEY
            - name: MYSQL_PASSWORD
              value: vault:secret/data/mysql#${.MYSQL_PASSWORD}
          resources:
            limits:
              memory: "128Mi"
              cpu: "100m"
```

## 3. Put Reloader to the test

To see the Reloader in action, first of all take a look at the logs to see information about which workload secrets are being collected and if any of them needs to be reloaded.

```bash
# watch reloader logs
kubectl logs -n bank-vaults-infra -l app.kubernetes.io/name=vault-secrets-reloader --follow
```

Now everything is set to try some things out with the Reloader:

1. Change a secret, observe the pods of the affected workloads ( `reloader-test-deployment-to-be-reloaded-xxx`, and `reloader-test-statefulset-0`) to be recreated (this might take up to a minute), check their logs for the updated secret.

    ```bash
    vault kv patch secret/mysql MYSQL_PASSWORD=totallydifferentsecret
    ```

    Also notice that there are two pods with the now changed `MYSQL_PASSWORD` injected into them not being restarted, for the following reasons:

    - the pod `reloader-test-deployment-no-reload-xxx` does not have the `alpha.vault.security.banzaicloud.io/reload-on-secret-change: "true"` annotation set
    - the pod `reloader-test-deployment-fixed-versions-no-reload-xxx` - although it does have the annotation - only uses versioned secrets, so they won't be reloaded for the latest version of the secret.

2. Change two secrets used in a workload, observe the previous pod to be recreated again, also that the pod `reloader-test-daemonset-xxx` only restarted once, although it uses both of these secrets. The number a workload got "reloaded" by the Reloader can be checked on the `alpha.vault.security.banzaicloud.io/secret-reload-count` annotation that is used to trigger a new rollout.

    ```bash
    vault kv patch secret/accounts/aws AWS_SECRET_ACCESS_KEY=s3cr3t2
    vault kv patch secret/dockerrepo DOCKER_REPO_PASSWORD=dockerrepopassword2
    
    # check the reload count after the new rollout has been completed
    kubectl get po -l app.kubernetes.io/name=reloader-test-daemonset -o jsonpath='{ .items[*].metadata.annotations.alpha\.vault\.security\.banzaicloud\.io/secret-reload-count }'
    ```

3. Update a workload to no longer have a secret, then change that secret, observe the workload not to be reloaded. This demonstrates that the collector worker keeps the list of watched workloads and secrets up-to-date whether they are newly created, updated or even removed.

    ```bash
    # delete MYSQL_PASSWORD from the initContainer and the container as well
    kubectl edit deployment reloader-test-deployment-to-be-reloaded
    
    vault kv patch secret/mysql MYSQL_PASSWORD=totallydifferentsecret2
    ```

4. Remove a secret from Vault, observe the error message in the logs of the Reloader.

    ```bash
    vault kv metadata delete secret/mysql

    # watch reloader logs, there should be similar error message soon:
    # time=xxx level=ERROR msg="Vault secret path secret/data/mysql not found" app=vault-secrets-reloader worker=reloader
    kubectl logs -n bank-vaults-infra -l app.kubernetes.io/name=vault-secrets-reloader --follow
    ```

You can tear down the test cluster with `make down` once you finished.
