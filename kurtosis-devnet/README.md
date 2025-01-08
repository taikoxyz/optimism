# Kurtosis-devnet support

## devnet specification

Due to sandboxing issues across repositories, we currently rely on a slight
superset of the native optimism-package specification YAML file, via go
templates.

So that means in particular that the regular optimism-package input is valid
here.

Additional custom functions:

- localDockerImage(PROJECT): builds a docker image for PROJECT based on the
  current branch content.

- localContractArtifacts(LAYER): builds a contracts bundle based on the current
  branch content (note: LAYER is currently ignored, we might need to revisit)

Example:

```yaml
...
  op_contract_deployer_params:
    image: {{ localDockerImage "op-deployer" }}
    l1_artifacts_locator: {{ localContractArtifacts "l1" }}
    l2_artifacts_locator: {{ localContractArtifacts "l2" }}
...
```

The list of supported PROJECT values can be found in `justfile` as a
PROJECT-image target. Adding a target there will immediately available to the
template engine.

## devnet deployment tool

Located in cmd/main.go, this tool handle the creation of an enclave matching the
provided specification.

The expected entry point for interacting with it is the corresponding
`just devnet SPEC` target.

This takes an optional 2nd argument, that can be used to provide values for the
template interpretation.

Note that a SPEC of the form `FOO.yaml` will yield a kurtosis enclave named
`FOO-devnet`

Convenience targets can be added to `justfile` for specific specifications, for
example:

```just
interop-devnet: (devnet "interop.yaml")
```

## devnet output

One important aspect of the devnet workflow is that the output should be
*consumable*. Going forward we want to integrate them into larger worfklows
(serving as targets for tests for example, or any other form of automation).

To address this, the deployment tool outputs a document with (hopefully!) useful
information. Here's a short extract:

```json
{
  "l1": {
    "name": "Ethereum",
    "nodes": [
      {
        "cl": "http://localhost:53689",
        "el": "http://localhost:53620"
      }
    ]
  },
  "l2": [
    {
      "name": "op-kurtosis-1",
      "id": "2151908",
      "services": {
        "batcher": "http://localhost:57259"
      },
      "nodes": [
        {
          "cl": "http://localhost:57029",
          "el": "http://localhost:56781"
        }
      ],
      "addresses": {
        "addressManager": "0x1b89c03f2d8041b2ba16b5128e613d9279195d1a",
        ...
      }
    },
    ...
  ],
  "wallets": {
    "baseFeeVaultRecipient": {
      "address": "0xF435e3ba80545679CfC24E5766d7B02F0CCB5938",
      "private_key": "0xc661dd5d4b091676d1a5f2b5110f9a13cb8682140587bd756e357286a98d2c26"
    },
    ...
  }
}
```

## further interactions

Beyond deployment, we can interact with enclaves normally.

In particular, cleaning up a devnet can be achieved using
`kurtosis rm FOO-devnet` and the likes.
