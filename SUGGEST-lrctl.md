# Proposition : `lrctl`, le client en ligne de commande de local-registry

## Objectif

Remplacer `scripts/upload.sh` et `scripts/publish-to-local-registry.sh` par un client Go, et couvrir aussi la consommation : chercher un produit, lire une version, télécharger et vérifier ses fichiers.

Un binaire Go apporte ce qu'un script bash ne peut pas faire :

- **Réutiliser le code du serveur.** La validation de `registry.yaml` et d'`artifacts.json` est faite en local, avant d'envoyer 500 Mo.
- **Ne plus dépendre de jq, de sha256sum ou de shasum.** Plus de problème de CRLF sous Windows.
- **Les mêmes binaires que le serveur** : Linux, macOS et Windows, en amd64 et arm64, signés.
- **Des envois en parallèle**, avec une barre de progression et des nouvelles tentatives.

Pas d'option `--token` : le token se verrait dans `ps` et dans l'historique du shell, comme pour le serveur.

## 1. Nom et forme

**`lrctl`** : un binaire client séparé, construit dans le même dépôt (`cmd/lrctl`) et publié par le même goreleaser.

- Le binaire serveur reste léger.
- Les CI et les postes n'installent que le client.
- L'autre option serait des sous-commandes du binaire serveur (`local-registry publish …`) : un seul binaire à distribuer, mais il mélange le serveur et le client (point 1).

## 2. Options globales (toutes les commandes)

| Option                          | Variable d'env                | Rôle                                                                              |
|---------------------------------|-------------------------------|-----------------------------------------------------------------------------------|
| `--url URL`                     | `LOCAL_REGISTRY_URL`          | Adresse du registre (obligatoire)                                                 |
| `--token-file F`                | `LOCAL_REGISTRY_UPLOAD_TOKEN` | Token d'upload. La variable a priorité, et `--token-file -` lit l'entrée standard |
| `--ca-file F`                   | `LOCAL_REGISTRY_CA_FILE`      | CA de l'entreprise, pour un HTTPS interne                                         |
| `-o, --output text\|json`       |                               | Sortie lisible ou JSON (pour `jq` et les CI)                                      |
| `--timeout D`                   |                               | Délai par requête (`5m` par défaut)                                               |
| `--retries N`                   |                               | Nouvelles tentatives sur erreur réseau ou `5xx` (3 par défaut)                    |
| `-q, --quiet` / `-v, --verbose` |                               | Moins ou plus de messages                                                         |
| `--no-color`                    | `NO_COLOR`                    | Sortie sans couleurs                                                              |

## 3. Commandes

| Commande                                    | Rôle                                                                          | Remplace                                    |
|---------------------------------------------|-------------------------------------------------------------------------------|---------------------------------------------|
| `publish <product> [artifacts.json \| DIR]` | Envoie les fichiers puis publie la version                                    | `upload.sh`, `publish-to-local-registry.sh` |
| `validate [registry.yaml] [--artifacts F]`  | Valide la fiche produit et `artifacts.json` en local, avec le code du serveur | `check-jsonschema`                          |
| `products [critères]`                       | Liste et recherche des produits (voir `SUGGEST.md`)                           | `curl /products \| jq`                      |
| `versions <product>`                        | Versions d'un produit, avec latest, pré-versions et versions dépréciées       | `curl /{product}/index`                     |
| `release <product> [version\|latest]`       | Affiche le `release.json` d'une version                                       | `curl /{product}/releases/…`                |
| `download <product> [version\|latest]`      | Télécharge et vérifie les fichiers                                            | `curl -O` + `sha256sum -c`                  |
| `ping`                                      | Vérifie l'accès au serveur (`/healthz`) et la validité du token               |                                             |
| `version`                                   | Version du client                                                             |                                             |
| `completion bash\|zsh\|fish\|powershell`    | Script d'autocomplétion                                                       |                                             |

## 4. `publish`

Les options reprises d'`upload.sh` gardent les mêmes noms et le même comportement :

| Option             | Rôle                                                                                             |
|--------------------|--------------------------------------------------------------------------------------------------|
| `--latest`         | Fait aussi de cette version la latest du produit                                                 |
| `--dry-run`        | Affiche ce qui serait envoyé, sans rien envoyer (pas besoin de token)                            |
| `--root DIR`       | Dossier auquel les chemins d'`artifacts.json` sont relatifs                                      |
| `--version V`      | Version à publier. Par défaut : `tag`, sinon `version`, de `metadata.json`                       |
| `--product-file F` | La fiche produit `registry.yaml`. Par défaut, celle de la racine, sinon celle du dossier courant |

Les nouvelles options :

| Option                       | Rôle                                                                                                                 |
|------------------------------|----------------------------------------------------------------------------------------------------------------------|
| `--changelog F`              | `CHANGELOG.md` à envoyer. Par défaut, celui qui est à côté d'`artifacts.json`                                        |
| `--exclude GLOB` (répétable) | Fichiers d'un DIR à ne pas envoyer (`*.log`, `.DS_Store`)                                                            |
| `--parallel N`               | Envois en parallèle (4 par défaut)                                                                                   |
| `--no-validate`              | Saute la validation locale, faite par défaut avant tout envoi                                                        |
| `--if-exists fail\|skip`     | Que faire si la version existe déjà : échec (par défaut), ou succès sans rien faire, pour rejouer une CI sans erreur |

Comportement :

- Les deux modes d'`upload.sh` restent : goreleaser (avec `artifacts.json`) et dossier simple (sans `artifacts.json`).
- L'ordre d'envoi ne change pas : les fichiers, puis `registry.yaml` et `CHANGELOG.md`, puis `artifacts.json` en dernier, puis la publication.
- Chaque fichier est envoyé avec `X-Checksum-Sha256`.
- Un DIR avec des sous-dossiers est refusé, comme aujourd'hui, avec la même indication d'archiver d'abord (`tar czf`).

## 5. `products`

Aligné sur le plan de recherche de `SUGGEST.md` :

| Option                     | Paramètre de l'API                   |
|----------------------------|--------------------------------------|
| `-q, --query TEXTE`        | `q`                                  |
| `--keyword K` (répétable)  | `keyword`                            |
| `--label k=v` (répétable)  | `label`                              |
| `--owner O`                | `owner`                              |
| `--license L`              | `license`                            |
| `--deprecated true\|false` | `deprecated`                         |
| `--prerelease true\|false` | `prerelease`                         |
| `--updated-since DATE`     | `updatedSince`                       |
| `--sort CLÉ`               | `sort`                               |
| `--limit N`                | `limit`                              |
| `--cursor C`               | `cursor`                             |
| `--all`                    | suit `next` jusqu'à la dernière page |

## 6. `download`

C'est la commande la plus utile côté serveurs et CI.

| Option                                  | Rôle                                                                                   |
|-----------------------------------------|----------------------------------------------------------------------------------------|
| `--os`, `--arch`                        | Choisit l'archive de cette plateforme. Par défaut, celle de la machine                 |
| `--kind archive\|binary\|package\|file` | Filtre par type de fichier                                                             |
| `--file NAME` (répétable)               | Fichiers précis, par nom                                                               |
| `--all`                                 | Tous les fichiers de la version                                                        |
| `-d, --dir DIR`                         | Dossier de destination (le dossier courant par défaut)                                 |
| `--extract`                             | Décompresse l'archive après vérification                                               |
| `--no-verify`                           | Saute le contrôle sha256. Déconseillé, puisque la spec demande aux clients de vérifier |
| `--verify-signature`                    | Vérifie la signature Sigstore du fichier de checksums, si `cosign` est installé        |

Exemple :

```sh
lrctl download billing-agent latest --extract -d /opt/billing-agent
```

- Le fichier est choisi à partir du `release.json` de la version, jamais en devinant son nom.
- Il est écrit dans un fichier temporaire, vérifié, puis renommé : un fichier corrompu n'arrive jamais à destination.

## 7. Codes de sortie (pour les CI)

| Code   | Signification                                               |
|--------|-------------------------------------------------------------|
| 0      | Succès                                                      |
| 1      | Erreur réseau ou serveur                                    |
| 2      | Mauvaise utilisation de la commande                         |
| 3      | Validation refusée (`422` du serveur, ou validation locale) |
| 4      | La version existe déjà (`409`)                              |
| 5      | Échec de la vérification d'un sha256 ou d'une signature     |
| 6      | Introuvable (`404`)                                         |

## 8. Points à valider

| #   | Question                         | Options                                                                                                                                                                                                                   | Recommandation                                                        |
|-----|----------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------------------------------------------------------------------|
| 1   | Forme                            | **A** binaire séparé `lrctl` · **B** sous-commandes du binaire serveur                                                                                                                                                    | **A**                                                                 |
| 2   | Bibliothèque CLI                 | **A** `flag` de la bibliothèque standard (zéro dépendance, comme le serveur), avec une petite couche maison pour les sous-commandes · **B** cobra (sous-commandes et autocomplétion plus simples, une dépendance de plus) | **A**                                                                 |
| 3   | Profils de connexion             | **A** plus tard : un fichier `~/.config/lrctl/config.yaml` avec plusieurs registres (`--context prod`), à la manière de kubectl · **B** dès la première version                                                           | **A** : les variables d'env suffisent en CI                           |
| 4   | `upload.sh`                      | **A** garder au moins une version, marqué comme déprécié, avec un message qui renvoie vers `lrctl` · **B** retirer tout de suite                                                                                          | **A**                                                                 |
| 5   | Périmètre de la première version | **A** `publish`, `validate`, `ping`, `version` · **B** tout, `download` et `products` compris                                                                                                                             | **A**, puis `download` ; `products` suit la recherche de `SUGGEST.md` |

