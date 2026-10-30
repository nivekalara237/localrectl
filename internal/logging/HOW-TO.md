# `internal/logging` : mode d'emploi

Ce paquet gère tout ce que `lrctl` écrit : le résultat d'une commande, les logs, les avertissements et les erreurs.

**La règle à respecter :**

- **stdout** ne reçoit que le résultat de la commande. En `-o json`, c'est un seul document JSON.
- **stderr** reçoit tout le reste : les logs, les avertissements et les erreurs.

Ainsi, `lrctl config -o json | jq .` marche toujours, même avec `-v`.

Une commande n'écrit donc jamais avec `fmt.Println`, `os.Stdout` ou `log.Printf`. Elle passe par le `Printer`.

---

## 1. Côté utilisateur

| Option             | Variable d'env                  | Clé du fichier de config | Effet                                                       |
|--------------------|---------------------------------|--------------------------|-------------------------------------------------------------|
| `-o, --output F`   | `LOCAL_REGISTRY_OUTPUT`         | `output`                 | `text` (par défaut) ou `json`, sans tenir compte de la casse |
| `-v, --verbose`    | `LOCAL_REGISTRY_VERBOSE`        | `verbose`                | Affiche aussi les logs `DEBUG`                              |
| `-q, --quiet`      | `LOCAL_REGISTRY_QUIET`          | `quiet`                  | N'affiche que `WARN` et `ERROR`                             |
| `--no-color`       | `LOCAL_REGISTRY_NO_COLOR`, `NO_COLOR` | `no-color`         | Sortie texte sans couleurs                                  |

- `-q` et `-v` ensemble donnent une erreur d'usage.
- En `-o json`, les logs et les erreurs sont aussi en JSON, sur stderr :
  ```json
    {"time":"…","level":"WARN","msg":"…","file":"c.yaml"}
    {"error":"config file nope.yaml: stat nope.yaml: no such file or directory"}
  ```
- **Codes de sortie :**
  - `0` : la commande a réussi ;
  - `1` : la commande a échoué ;
  - `2` : erreur d'usage (option, variable ou fichier de config incorrect).

---

## 2. Comment c'est branché

```text
main ── rootCmd.ExecuteC()
         │
         ├─ PersistentPreRunE (internal/cli/root.go)
         │    1. config.LoadConfig(cmd.Flags(), &notes)   → *Config (jamais nil) + erreur
         │    2. logging.NewPrinter(out, err, cfg.OutputOptions())
         │    3. cmd.SetContext(logging.With(ctx, p))     ← même si la config est refusée
         │       + config.With(ctx, cfg)                  ← seulement si elle est valide
         │    4. notes.Flush(p.Logger())                  ← les messages gardés par LoadConfig
         │
         ├─ RunE de la commande
         │    p := logging.From(cmd.Context())
         │    p.Logger().Debug(…) / p.Result(…)
         │
         └─ en cas d'erreur, dans main :
              logging.From(cmd.Context()).Error(err)      → texte ou {"error": …} sur stderr
              os.Exit(2) si config.IsUsageError(err), sinon os.Exit(1)

```

Les options sont déclarées une seule fois, sur la commande racine :

```go
config.AddFlags(root.PersistentFlags())
logging.AddArgs(root.PersistentFlags())    // -v, -q
logging.AddLogArgs(root.PersistentFlags()) // -o
```

Une nouvelle commande n'a rien à brancher. Il suffit de l'ajouter avec `root.AddCommand(...)`.

---

## 3. Écrire une commande

### 3.1 Le résultat : `Result`

On construit **une struct** avec des tags `json` en snake_case, et une fonction qui l'affiche en texte. `Result` choisit le format selon `-o`.

```go
type pingResult struct {
	URL       string `json:"url"`
	Status    int    `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
	TokenOK   bool   `json:"token_ok"`
}

func newPingCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "ping",
		Short: "Check the access to the server and the token",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := logging.From(cmd.Context())
			cfg := config.From(cmd.Context())
			if cfg == nil {
				return errors.New("the configuration is not loaded: run this command from the root command")
			}

			res, err := ping(cmd.Context(), cfg, p.Logger())
			if err != nil {
				return err // main l'affiche au bon format
			}
			return p.Result(res, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "%s: %d in %dms, token ok: %v\n",
					res.URL, res.Status, res.LatencyMs, res.TokenOK)
				return err
			})
		},
	}
}
```

- **En JSON**, `Result` écrit `v` indenté de 2 espaces, et la fonction texte est ignorée.
- **En texte**, `Result` appelle la fonction texte. Si elle est `nil`, il fait un `fmt.Fprintln(Out, v)`, ce qui convient pour une simple chaîne.
- **Appelez `Result` une seule fois par commande.** Deux appels en JSON produiraient deux documents, et `jq` ne saurait plus les lire.
- **Pour une liste**, renvoyez une struct qui l'enveloppe, comme `{"products": [...]}`, plutôt que le tableau seul. Vous pourrez ainsi y ajouter des champs plus tard sans casser les scripts qui la lisent.
- **Une valeur absente vaut `nil`**, ce qui donne `null` en JSON. `lrctl config` l'affiche `-` en texte (voir `orNil` dans `internal/cli/config.go`).

### 3.2 Les erreurs : les renvoyer, pas les afficher

Une commande **renvoie** son erreur, et c'est `main` qui l'affiche. On n'appelle jamais `p.Error` soi-même : l'erreur s'afficherait deux fois.

| Situation                                       | Ce qu'on renvoie                    | Code de sortie |
|-------------------------------------------------|-------------------------------------|---------------|
| Argument ou valeur incorrect donné par l'utilisateur | `config.Usage(fmt.Errorf(...))` | 2             |
| Échec réel (réseau, serveur, fichier)           | `fmt.Errorf("...: %w", err)`        | 1             |

`config.IsUsageError` retrouve une erreur d'usage même si elle a été enveloppée plus loin avec `%w`.

### 3.3 Les logs : `p.Logger()`

C'est un `*slog.Logger` qui écrit sur stderr. On l'utilise avec des paires clé/valeur, jamais avec `fmt.Sprintf` dans le message :

```go
log := p.Logger()
log.Debug("request", "method", "GET", "url", u, "status", resp.StatusCode, "duration", d)
log.Info("uploading", "file", name, "size", size)
log.Warn("retrying", "attempt", n, "of", cfg.Retries, "err", err)
```

| Niveau  | Affiché                  | À utiliser pour                                                        |
|---------|--------------------------|------------------------------------------------------------------------|
| `Debug` | avec `-v` seulement      | Les détails : requêtes HTTP, fichiers lus, source de chaque réglage    |
| `Info`  | par défaut, pas avec `-q` | Le déroulement d'une commande longue (« uploading », « published »)    |
| `Warn`  | toujours                 | Un problème qui n'arrête pas la commande (droits du fichier, nouvelle tentative) |
| `Error` | toujours                 | Rarement : une erreur qui arrête la commande est **renvoyée** (voir 3.2) |

**Les messages et les clés :**
- Le message est court, en anglais, et sans valeur variable : `"request"`, pas `"GET https://…"`.
- Les clés sont stables, en minuscules avec des tirets bas : `file`, `url`, `status`, `duration`. Les CI filtrent dessus en `-o json`.
- Pour une erreur, on utilise la clé `err`.

### 3.4 Les secrets

`NewLogger` remplace par `[REDACTED]` (`logging.Redacted`) la valeur de tout attribut dont le **nom** contient `token`, `authorization` ou `password`, quelle que soit la casse. Cela s'applique aussi dans un `slog.Group`.

```go
log.Debug("request", "authorization", "Bearer abc") // → authorization=[REDACTED]
```

**Ce filet ne regarde que le nom de la clé.** Un secret placé dans le message, ou sous une autre clé (`"header", req.Header`), passe en clair. On ne logge donc jamais de secret volontairement.

Pour indiquer qu'un token est présent sans le montrer, utilisez une clé qui ne contient pas `token`, par exemple `"auth", "set"`. Une clé comme `token_set` serait elle-même masquée.

En résultat (`Result`), le token n'apparaît que masqué avec `maskToken` (`internal/cli/config.go`).

### 3.5 Les couleurs

```go
if p.UseColor() {
	status = "\033[32m" + status + "\033[0m"
}
```

`UseColor()` renvoie `false` dans trois cas : en `-o json`, avec `--no-color`, ou si `NO_COLOR` est définie et non vide (https://no-color.org). On ne colore que le texte écrit dans la fonction texte de `Result`, jamais le JSON ni les logs.

---

## 4. Écrire un log avant que le logger existe : `Notes`

Le niveau du logger dépend de `-v` et `-q`. Or ces réglages peuvent venir du fichier de config, donc ils ne sont connus qu'**après** `LoadConfig`. Le code qui s'exécute avant le logger garde donc ses messages dans des `Notes` :

```go
func LoadConfig(fs *pflag.FlagSet, notes *logging.Notes) (*Config, error) {
	notes.Debug("config file found", "path", file)
	notes.Warn("the config file holds the upload token but is readable by other users",
		"file", file, "fix", "chmod 600 "+file)
	notes.Add(slog.LevelInfo, "…") // n'importe quel niveau
	…
}
```

`PersistentPreRunE` les envoie au logger avec `notes.Flush(p.Logger())`. Elles sont écrites dans leur ordre, filtrées par le niveau, puis la liste est vidée. Dans une commande, on n'a pas besoin des `Notes` : on utilise directement `p.Logger()`.

---

## 5. Tester

### Une commande, de bout en bout (recommandé)

C'est la méthode utilisée dans `internal/cli/cli_test.go` :

```go
func TestPingJSON(t *testing.T) {
	isolate(t) // dossier temporaire = $HOME, pas de LOCAL_REGISTRY_* ni NO_COLOR
	r := mustRun(t, "ping", "-o", "json", "-v")

	var got pingResult
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, r.stdout)
	}
	if !strings.Contains(r.stderr, `"msg":"request"`) { … }  // les logs -v sont sur stderr
	if strings.Contains(r.stdout+r.stderr, token) { … }      // le token n'apparaît nulle part
}
```

Ce qu'il faut vérifier pour chaque nouvelle commande :
- [ ] `-o json` : stdout est **un seul** document JSON valide (`json.Valid`).
- [ ] `-o text` : la sortie attendue.
- [ ] `-v` : les détails sont sur stderr, et stdout ne change pas.
- [ ] `-q` : stderr est vide quand il n'y a ni avertissement ni erreur.
- [ ] Une mauvaise valeur donne `config.IsUsageError(err) == true`.
- [ ] Aucun secret n'apparaît dans stdout ni dans stderr.

### Le `Printer` seul

```go
var out, errw bytes.Buffer
p := logging.NewPrinter(&out, &errw, logging.Options{Format: logging.JSON, Verbose: true})
ctx := logging.With(context.Background(), p)
// … appeler le code qui fait logging.From(ctx) …
```

Si aucun `Printer` n'est rangé dans le context, `logging.From` renvoie un `Printer` texte sur `os.Stdout` et `os.Stderr`. Un test qui oublie `With` affichera donc sa sortie dans le terminal au lieu de la capturer.

---

## 6. Utiliser le paquet ailleurs que dans `lrctl`

Par exemple dans un autre binaire, ou sans cobra :

```go
fs := pflag.NewFlagSet("tool", pflag.ExitOnError)
logging.AddArgs(fs)
logging.AddLogArgs(fs)
_ = fs.Parse(os.Args[1:])

out, _ := fs.GetString("output")
verbose, _ := fs.GetBool("verbose")
quiet, _ := fs.GetBool("quiet")
opts := logging.Options{Format: logging.Format(out), Verbose: verbose, Quiet: quiet}
if err := opts.Validate(); err != nil {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
p := logging.NewPrinter(os.Stdout, os.Stderr, opts)
```

**Appelez `Validate()` avant `NewPrinter`.** `NewPrinter` remplace sans rien dire un format invalide par `text`, pour pouvoir quand même afficher une erreur.

---

## 7. Référence

| Élément                                      | Rôle                                                                   |
|----------------------------------------------|------------------------------------------------------------------------|
| `Format`, `Text`, `JSON`                     | Le format de sortie                                                    |
| `ParseFormat(s)`                             | Lit une valeur de `--output`. `""` donne `Text`, une valeur inconnue une erreur |
| `Options{Format, Verbose, Quiet, NoColor}`   | Les réglages de sortie                                                 |
| `Options.Validate()`                         | Refuse un format inconnu et `-q` avec `-v`, et renvoie toutes les erreurs ensemble |
| `Options.Level()`                            | `Debug` avec `-v`, `Warn` avec `-q`, `Info` sinon                      |
| `NewPrinter(out, err, opts)`                 | Le `Printer`, avec son logger sur `err`                                |
| `Printer.Result(v, text)`                    | Le résultat sur stdout (JSON, ou la fonction texte)                    |
| `Printer.Error(err)`                         | L'erreur sur stderr (`{"error":…}` en JSON). Réservé à `main`          |
| `Printer.Logger()`                           | Le `*slog.Logger` sur stderr                                           |
| `Printer.UseColor()`                         | Si la sortie texte peut être colorée                                   |
| `With(ctx, p)`, `From(ctx)`                  | Ranger et retrouver le `Printer` (avec un repli texte s'il n'y en a pas) |
| `NewLogger(w, opts)`                         | Un logger texte ou JSON au bon niveau, qui masque les secrets          |
| `Redacted`                                   | `"[REDACTED]"`                                                         |
| `Notes`, `Note`, `Add`, `Debug`, `Warn`, `Flush` | Garder des messages en attendant que le logger existe              |
| `AddArgs(fs)`                                | Déclare `-v` et `-q`                                                   |
| `AddLogArgs(fs)`                             | Déclare `-o`                                                           |

## 8. Pièges connus

- **Les erreurs détectées par cobra restent en texte, même avec `-o json`.** Une option inconnue ou un argument en trop sont refusés avant `PersistentPreRunE`, donc avant que le `Printer` existe. Le code de sortie reste 2.
- **`config.From(ctx)` renvoie `nil` si la config a été refusée**, ou si la commande est lancée sans la commande racine. Testez ce cas et renvoyez une erreur plutôt que de risquer un panic.
- **N'ajoutez pas de `fmt.Println` de debug.** Il écrit sur stdout et casse `-o json`. Utilisez `p.Logger().Debug(...)` et lancez la commande avec `-v`.
