# State-Backends

Panko legt den OpenTofu-State standardmäßig als Secret im Namespace `panko-state-<team>`
ab. Teams können stattdessen ein vom Betrieb freigegebenes Ziel wählen. Der Aufbau
kombiniert einen eigenen Entwurf mit einem unabhängigen Review: Betriebskatalog, einmalige Bindung,
Prüfung des tatsächlich initialisierten Backends und eine Migration nur durch Administratoren.

## Modell

| Ressource | Verantwortlich | Inhalt |
| --- | --- | --- |
| `BackendProfile` | Betrieb, im Team-Namespace | Freigegebenes Ziel: Typ, Bucket, Region, Präfix, Endpunkt |
| `StateBackend` | Team, über das Gateway | Auswahl eines Profils und des Credential-Secrets |
| `Database.spec.stateBackendRef` | Team, optional, unveränderlich | Explizite Auswahl eines StateBackends |
| `Database.status.stateBackend` | Panko, nur per Migration änderbar | Die gebundene, maßgebliche Konfiguration |

Auflösung bei der ersten Reconciliation einer neuen Ressource:

1. `spec.stateBackendRef`, falls gesetzt.
2. Sonst das StateBackend `default` im Namespace.
3. Sonst das eingebaute Kubernetes-Backend.

Fehlt eine explizite Referenz, ihr Profil oder ihre Credentials, wird die Ressource `Failed`
und `status.stateBackendError` nennt den Grund. Es gibt keinen stillen Rückfall auf ein
anderes Backend. Ressourcen, die vor Einführung der Bindung bereits liefen, werden immer an
ihren bisherigen Kubernetes-Ort gebunden, auch wenn inzwischen ein `default` existiert.

Die Bindung wird in einer eigenen Reconciliation gespeichert, bevor der erste Job startet.
Spätere Änderungen an StateBackends, Profilen oder dem `default` verschieben bestehende
Ressourcen nicht. Den Teams stehen nur `spec`-Felder offen; Gateway und RBAC erlauben
weder Annotationen noch den Status-Subresource.

S3-Schlüssel leitet Panko selbst ab: `<keyPrefix>/<installation>/<uid>/terraform.tfstate`.
Die Installations-ID liegt in der ConfigMap `infra-runners/panko-installation` und wird
einmalig erzeugt. Locking über `use_lockfile` ist immer aktiv.

## Sicherungen im Runner

- Der Runner schreibt das Backend immer als `panko_backend_override.tf.json`. Ein Backend
  im Modul wird ersetzt; fehlt es, verhindert das Override lokalen State im Job.
- Module mit eigenen Override-Dateien (`override.tf`, `*_override.tf[.json]`) werden
  abgelehnt, weil sie Pankos Override in alphabetischer Reihenfolge übersteuern würden.
- Nach `tofu init` vergleicht der Runner `.terraform/terraform.tfstate` mit der Bindung.
  Typ oder Einstellung weichen ab: kein Plan, kein Apply.
- Ein Checkpoint (`panko-checkpoint-<uid>` im State-Namespace) speichert `lineage` und
  `serial` nach jedem Lauf, auch nach Fehlern. Vor dem nächsten Lauf blockiert der
  Runner, wenn etablierter State fehlt, die Lineage wechselt oder die Serial zurückgeht.
  Wiederherstellen des States oder bewusstes Entfernen des Checkpoints gibt die
  Ressource wieder frei.
- Die Prozessumgebung entsteht aus einer Allowlist. Umgebungsvariablen wie
  `TF_CLI_ARGS`, `AWS_PROFILE` oder Proxys erreichen OpenTofu nicht. S3 erhält nur
  `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` und optional `AWS_SESSION_TOKEN` aus dem
  gebundenen Secret; Metadaten-Credentials sind abgeschaltet.
- `tofu output -json` wird nicht mehr ausgegeben. State wird nur im Speicher gelesen.
- Destroy verwendet die gebundene Konfiguration. Beim Kubernetes-Backend entfernt der
  Runner State und Lease, bei S3 bleibt der leere State für Audit und Wiederherstellung.

Das PostgreSQL-Backend ist bewusst nicht zugelassen: Der Runner übergibt `PGPASSWORD`
an den PostgreSQL-Provider, und das `pg`-Backend liest dieselbe Variable. Ein Team-Ziel
könnte so das Infrastruktur-Administratorpasswort erhalten.

## Lab-S3 (VersityGW)

Mit `installation.s3` (`"s3": true` in `panko.json`, Standard in `panko dev up`) startet das
Chart [VersityGW](https://github.com/versity/versitygw) als S3-Gateway in `infra-backends`. Das Lab nutzt VersityGW, weil es bedingte Schreibvorgänge
(`If-None-Match`) korrekt umsetzt; ohne diese wäre `use_lockfile` wirkungslos.
Der Setup-Job erzeugt je Team die Schlüssel, der Job `s3-onboard-<digest>` legt je Team einen
eigenen S3-Benutzer mit Rolle `user` und einen eigenen Bucket `panko-state-<team>` an, und das
Chart rendert das BackendProfile `lab-s3` je Team. Das Secret `panko-state-s3` im Team-Namespace enthält
dessen Schlüssel. Andere Teams erhalten auf den Bucket keinen Lese-, Listen- oder
Schreibzugriff. Der Root-Schlüssel (`infra-backends/s3-admin`) dient nur dem Onboarding.

Ein bestehendes Lab erhält den S3-Gateway beim nächsten `make deploy` bzw. `panko install` mit
aktiviertem `s3`.

Ein Team wählt sein Lab-Ziel so:

```yaml
apiVersion: platform.example.com/v1
kind: StateBackend
metadata: {name: default, namespace: team-a}
spec:
  profileRef: {name: lab-s3}
  credentialsSecretRef: {name: panko-state-s3}
```

Der State enthält die erzeugten Datenbankpasswörter. Wer einen Bucket lesen kann, kann
diese Passwörter lesen; wer ihn schreiben kann, beeinflusst, was privilegierte Provider
später ändern. Fremden State per `terraform_remote_state` zu lesen ist deshalb kein
vorgesehener Weg zwischen Projekten; Ergebnisse werden als Kubernetes-Objekte geteilt.

## Migration

Nur Administratoren verschieben State, mit einer ID, die bei Abbruch erneut verwendet wird:

```sh
.local/bin/admin --kubeconfig .local/kubeconfig migrate-backend \
  --namespace team-a --name orders --state-backend default --migration-id m-20261024
```

| Phase | Bedeutung |
| --- | --- |
| `Frozen` | Annotation `platform.example.com/state-maintenance` gesetzt; der Hook startet keine Jobs, laufende Jobs enden regulär |
| `Copied` | Migrations-Job hat gesichert, kopiert und beide Seiten erneut gelesen |
| `Committed` | `status.stateBackend` per resourceVersion-geschütztem Update umgestellt |
| `Resumed` | Bindung bestätigt, Annotation entfernt |

Der Migrations-Job arbeitet mit reinen Backend-Konfigurationen ohne Modul und ohne
Infrastruktur-Credentials. Provider, die der gespeicherte State referenziert, werden über
das Lockfile des Moduls geprüft; ein dort nicht enthaltener Provider bricht ab. Vor dem
Kopieren entsteht eine Sicherung `panko-backup-<uid>-<id>`. Kopiert wird mit OpenTofus
eigener Backend-Migration (`tofu init -migrate-state`), nur in ein leeres Ziel.

OpenTofu vergibt dem State im Ziel eine neue Lineage. Panko vergleicht deshalb Ressourcen
und Outputs beider Seiten und überträgt den Checkpoint nach der verifizierten Kopie auf das
Ziel. Ein Ziel mit anderem Inhalt wird nie überschrieben; ein Ziel mit gleichem Inhalt gilt
beim Fortsetzen als bereits kopiert. Bis zum Commit bleibt die Quelle maßgeblich, danach das
Ziel. Der Commit ersetzt `status.stateBackend` als Ganzes per JSON Patch mit
resourceVersion-Test. Metacontroller ersetzt den Status ohne Versionsprüfung, deshalb
bestätigt das Kommando die neue Bindung über mehrere Reconciliations und setzt sie während
des Freeze notfalls erneut.

Die Quelle bleibt nach der Migration erhalten. Entfernen ist ein bewusster
Administratorschritt, ebenso das Löschen der Sicherung und des Datensatzes
`panko-migration-<uid>`. `spec.stateBackendRef` bleibt unverändert; maßgeblich ist der Status.

## Grenzen

- Nur `kubernetes` und `s3`. Weitere Typen brauchen eigene Adressierung, Credentials und Prüfungen.
- Migrationen zwischen zwei S3-Zielen mit verschiedenen Schlüsseln werden abgelehnt: Beide
  Seiten lesen dieselben `AWS_*`-Variablen. Kubernetes → S3 und S3 → Kubernetes funktionieren.
- Egress der Runner-Pods ist nicht auf freigegebene Ziele beschränkt; normale
  NetworkPolicies können nicht nach Hostnamen filtern.
- Kein Ausführungsjournal: Bricht ein Job ab, nachdem Infrastruktur geändert, aber bevor
  State geschrieben wurde, kann Panko das nicht erkennen.
- Workload Identity statt gespeicherter Schlüssel ist noch nicht umgesetzt.
- Checkpoints erkennen Fehler gegenüber dem zuletzt gesehenen Stand, authentifizieren aber
  keinen böswilligen Schreiber mit Zugriff auf den Bucket.
