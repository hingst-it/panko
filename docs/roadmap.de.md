# Roadmap: Panko für den persönlichen Betrieb

Ziel: Eine Person betreibt Panko für sich selbst, etwa im Homelab oder für ein Nebenprojekt.
Die Roadmap entstand aus zwei unabhängigen Entwürfen (Claude, Codex) und einem gegenseitigen
Review. Mehrmandantenfähigkeit für fremde Teams und Unternehmensanforderungen sind kein Ziel.

## Meilenstein 1: erste nutzbare lokale Version

Erreicht, wenn eine frische Installation eine nützliche Ressource anlegen, einen Fehler
verständlich melden und gezielt wiederholen, Zugangsdaten bereitstellen, eine geprobte
Wiederherstellung überstehen und sauber löschen kann, jeweils über dokumentierte Abläufe.

| Nr. | Thema | Größe | Stand |
| --- | --- | --- | --- |
| 1 | Verständliche Fehler und explizites Wiederholen | L | umgesetzt |
| 1b | Pull-Through-Mirror für Provider | M | umgesetzt |
| 2 | Geprobte Sicherung und Wiederherstellung | M | umgesetzt, Probe-Wiederherstellung bestanden |
| 3 | Reproduzierbare persönliche Installation | M | umgesetzt |
| 4 | Alltagsbefehle (CLI) | M | umgesetzt |

### 1. Verständliche Fehler und explizites Wiederholen

Heute meldet ein Fehlschlag nur „Apply failed; inspect job logs" (`internal/hook/hook.go`);
über das Gateway sind keine Logs erreichbar, und Wiederholen erfordert eine Spec-Änderung.

- Jede Phase meldet einen festen Fehlercode mit sicherer Meldung und nächstem Schritt:
  Modulabruf (git-sync), Engine-Kopie, Backend-Initialisierung, Backend-Prüfung,
  Checkpoint, Provider-Ausführung, Aufräumen. Init-Container-Fehler gehören dazu.
- Ein begrenzter Ausführungsdatensatz im Status: Versuch, Job, Generation, Modulrevision,
  Zeitpunkte, Phase, Fehlercode, Änderungszahlen des Plans. Freitext aus OpenTofu wird
  nicht ungefiltert veröffentlicht.
- Apply und Destroy lassen sich über eng definierte Gateway-Aktionen wiederholen, ohne die
  Spec zu ändern und ohne beliebige Annotationen freizugeben.
- Fehlerdaten überleben die Job-TTL.

Abnahme: Ein gezielt herbeigeführter Fehler je Phase erscheint mit passendem Code im Status
und ist über das Gateway lesbar und wiederholbar.

### 1b. Pull-Through-Mirror für Provider

Jeder Job lädt Provider neu herunter; ein abgebrochener Download von GitHub hat im Lab
bereits einen Job scheitern lassen, auch Destroys sind betroffen.

- Kleiner Cache-Dienst im Cluster, der OpenTofus Network-Mirror-Protokoll spricht und bei
  Cache-Miss aus der Registry lädt.
- HTTPS-Mirror mit eigener CA; der Runner vertraut Systemwurzeln plus `/mirror-ca`
  über `SSL_CERT_DIR=/etc/ssl/certs:/mirror-ca`. Die CA wird als ConfigMap je Team verteilt.
- Vor `init` prüft der Runner `/healthz` mit zwei Sekunden Timeout, auch bei Migrationen.
  Erreichbar: CLI-Konfiguration mit `network_mirror`. Nicht erreichbar: direkter
  Registry-Zugriff und ein strukturiertes Warnereignis. OpenTofu verlangt HTTPS und
  fällt bei einem unerreichbaren konfigurierten Mirror nicht auf `direct` zurück.
  Fällt der Mirror erst nach der Probe aus, kann `init` deshalb scheitern.
- Indizes haben eine begrenzte TTL; Versionsmetadaten und ZIPs bleiben unverändert.
  ZIPs werden vor dem atomaren Speichern gegen die Registry-SHA-256 geprüft.
  Das Modul-Lockfile prüft die Provider zusätzlich im Runner.
- Nur Downloads laufen über den Mirror. Kein allgemeiner HTTPS-Forward-Proxy: Er könnte
  verschlüsselten Verkehr nicht cachen und würde Provider-, Backend- und API-Verbindungen
  beeinflussen. Die Runner-Umgebung entfernt `HTTP(S)_PROXY` weiterhin.

Abnahme: Zweiter Job lädt keine Provider mehr aus dem Internet; ein gestoppter Mirror
verhindert keinen Apply oder Destroy, wenn er bereits bei der Probe nicht erreichbar ist.

### 2. Geprobte Sicherung und Wiederherstellung

- Sicherung im Stillstand: CR-Identitäten und Bindungen, State, Checkpoints,
  Installations-ID, benötigte Secrets und die tatsächlichen Datenbankinhalte. Ein
  wiederhergestellter State allein stellt keine Daten wieder her.
- Lab-S3 liegt auf einem Cluster-Volume und ist damit nicht unabhängig dauerhaft.
- Wiederherstellung in ein getrenntes Lab als Probe; Checkpoint-Konflikte werden angezeigt,
  Entfernen bleibt eine bewusste Aktion.

Abnahme: Wiederherstellung in ein frisches Lab, danach Update und Destroy einer
wiederhergestellten Ressource ohne Neuanlage der Datenbank.

### 3. Reproduzierbare persönliche Installation

- Teams konfigurierbar statt fest `team-a`/`team-b` (`internal/admin/rbac.go`,
  `cluster/clean.sh`); ein Befehl legt ein Team samt Namespaces, RBAC, Token und
  S3-Zugang an.
- Eine Konfigurationsdatei für Teams, Modulrepository und -revision, Images und optionale
  Lab-Bestandteile; Lab-S3 optional, Kubernetes-State bleibt Standard.
- Dokumentierter Upgrade-Pfad. Helm-Chart und veröffentlichte Images erst bei Bedarf.

Abnahme: Installation mit nur einem selbst benannten Team aus der Konfigurationsdatei,
danach Upgrade auf eine neuere Revision ohne zusätzliche Applies.

### 4. Alltagsbefehle (CLI)

- Schlanke CLI über dem Gateway: anlegen, warten, anzeigen, wiederholen, verbinden, löschen.
- `doctor` prüft Konfiguration, RBAC, Modulzugriff und Backends.
- Zugangsdaten über einen bewussten lokalen Befehl, der das Connection-Secret liest; das
  Gateway erhält keinen allgemeinen Secret-Zugriff. Token-Rotation als Befehl.

Abnahme: Der komplette Lebenszyklus einer Datenbank inklusive Verbindungsaufbau ohne
kubectl und ohne curl.

## Danach

| Nr. | Thema | Größe | Anmerkung | Stand |
| --- | --- | --- | --- | --- |
| 3b | Modul-Mirror im Cluster | M | git-sync liest vom Mirror; der Deploy-Key liegt nur noch dort statt in jedem Team-Namespace | umgesetzt |
| 3c | Egress-Regeln für Runner | M | Erst nach 1b und 3b. Panko erzeugt NetworkPolicies aus deklarierten Zielen; Dienste werden auf ihre Pods aufgelöst, externe Ziele als CIDR und Port. Test muss auch Verweigerung zeigen | umgesetzt, optional (`"egress": true`) |
| 5 | Zweiter, echter Ressourcentyp | L | Mit eigenem Credential- und Output-Vertrag; der Runner lädt heute immer das PostgreSQL-Admin-Passwort | offen: erst mit einem tatsächlich benötigten Modul |
| 6 | Drift-Prüfung und kontrollierte Modul-Rollouts | L | Lesender Plan auf Abruf; Revisionswechsel pro Ressource statt flottenweit | umgesetzt |
| 7 | Minimale Oberfläche | M, Graph +M | Erst Inspektor (Ressourcen, Phase, letzte Ausführung, Fehlercode); Graph als zweiter Reiter aus `tofu graph`, als bereinigtes Artefakt ohne Attribute, Outputs und Instanzschlüssel, mit Team-Token und UID-Prüfung | umgesetzt (Inspektor und Graph) |

## Meilenstein 2: Entfernte Runner

Erreicht, wenn eine Ressource Infrastruktur in einem Netz provisioniert, das der Cluster
nicht erreicht, über einen Runner, der dort als Container, als Deployment in einem anderen
Cluster oder als Systemdienst läuft, mit denselben Status-, Fehler- und Löschverträgen wie
Jobs im Cluster. Gegen eine emulierte Cloud-API nachgewiesen.

| Nr. | Thema | Größe | Stand |
| --- | --- | --- | --- |
| 8a | Runner-Protokoll, Runner-Pools und Agent als Container | XL, in prüfbaren Teilschritten | offen |
| 8b | Paketierung als Systemdienst und für entfernte Cluster | M | offen, nach 8a |
| 8c | Nachweis gegen eine emulierte Cloud-API (Moto) | M | offen, mit 8a |

### 8. Entfernte Runner nach dem Pull-Modell

Heute erzeugt der Hook für jede Ausführung einen Job im Team-Namespace; der Runner muss
also im Cluster laufen und alle Ziele von dort erreichen. Entfernte Runner arbeiten wie
selbst gehostete GitHub-Actions-Runner: Ein Agent baut nur ausgehende HTTPS-Verbindungen
auf, holt sich eine Ausführung, führt OpenTofu lokal aus und meldet das Ergebnis zurück.
Anders als dort führt der Agent keine beliebigen Schritte aus, sondern nur das gepinnte
Modul mit dem Spec-Snapshot der Ressource. Der Entwurf wurde von Codex gegen den Code
geprüft; die Punkte unten enthalten dessen Befunde.

**Pools und Bindung**

- Runner-Pools deklariert der Betreiber in `panko.json`: Name, erlaubte Teams und optional
  ein eigenes Modul (Repository, Revision, Pfad), etwa für eine andere Cloud. Ohne Pool bleibt
  der Job im Cluster der Standard; Teams mit und ohne Pool laufen nebeneinander.
- Eine Ressource bindet ihren Pool einmalig, wie das State-Backend (`status.stateBackend`).
  Team-Vorgaben gelten nur für neue Ressourcen. Ein Pool lässt sich nicht entfernen oder
  umhängen, solange Ressourcen an ihn gebunden sind; ein Wechsel ist eine bewusste Migration.
- Das Löschen behält Pool und Modulrevision der Erstellung, wartet auf laufende
  Ausführungen und bleibt blockiert, solange kein Agent des Pools verfügbar ist.

**Ausführungsobjekt `Run`**

- Für Ressourcen eines entfernten Pools erzeugt der Hook statt eines Jobs ein Kind-Objekt
  `Run` (eigene CRD, in der CompositeController-Definition deklariert). `spec` ist
  unveränderlich: Aktion, Generation, Modulrevision und Modulpfad, Versuch, Token,
  Spec-Snapshot, nicht geheime Backend-Bindung, erwarteter Checkpoint. Lease und Ergebnis
  stehen im Status-Subresource und gehören dem Runner-Endpunkt.
- Deterministische Namen, Owner-Referenzen und Aufbewahrung wie bei Jobs; aktive Runs bleiben
  in jeder Hook-Antwort, weil Metacontroller fehlende Kinder löscht.
- Jobs und Runs werden gemeinsam serialisiert, auch Pläne und Wartung. Ein Claim ist ein
  Compare-and-Swap auf den Status; Versuche und Tokens gelten dauerhaft als verbraucht.
- `Run` wird nicht über das Team-Gateway veröffentlicht (keine automatische CRD-Discovery,
  keine erzeugten Schreibrechte).

**Runner-Endpunkt und Protokoll**

- Eigener kleiner Dienst mit eigenem Service-Account, nicht das Team-Gateway. Er darf
  Run-Status, Checkpoints, Connection-Secrets und Graph-Artefakte schreiben, nicht die
  breiten Rechte des heutigen Runners. Alle Ziele (Namespace, Secret-Name, Schlüssel,
  Owner-Referenz) leitet er serverseitig aus dem Run ab; Angaben des Agenten dazu werden
  abgewiesen.
- Versioniertes Protokoll: `claim` (Long-Poll), `heartbeat`, `commit`. Jede Meldung ist an
  Installation, Ressourcen-UID und Identität, Run-UID, Versuch, Agent und Lease-Epoche
  gebunden. Begrenzte Nutzlasten, streng typisierte Zahlen, nur gültige Kombinationen aus
  Aktion und Ergebnis; Meldungstexte erzeugt der Endpunkt aus dem Fehlercode.
- Ergebnis, Checkpoint und Zugangsdaten werden in einem wiederaufnehmbaren Ablauf
  festgeschrieben (`Committing`, dann Endzustand), mit Idempotenzschlüssel: Eine identische
  Wiederholung erhält dieselbe Bestätigung, eine abweichende oder veraltete überschreibt nie
  neuere Daten. Checkpoints kommen auch nach fehlgeschlagenen Teil-Applies zurück und
  verschwinden erst nach erfolgreichem Destroy.
- Drift-Pläne behalten Token-Deduplizierung, Serialisierung und die Behandlung fehlender
  Berichte und schreiben weder Zugangsdaten noch Checkpoints. Graph-Artefakte bleiben
  bereinigt und begrenzt; Identität und Revision setzt der Endpunkt, veraltete Uploads werden
  abgewiesen.

**Lease und Wiederherstellung**

- Heartbeat etwa alle 15 s, Ablauf nach 60 s, dazu eine maximale Ausführungsdauer. Verliert
  der Agent den Heartbeat, bricht er den gesamten Prozessbaum ab.
- Ein abgelaufener Lease meldet `interrupted`; wiederholt wird nur explizit über `retry`.
- Die S3-State-Sperre (`use_lockfile`) verhindert nur gleichzeitige kooperierende
  OpenTofu-Läufe; sie schützt nicht vor einem abgeschnittenen Agenten, der weiter
  Provider-APIs aufruft, und ein getöteter Prozess kann sie zurücklassen. Panko löst sie nie
  automatisch; die Wiederherstellung ist ein dokumentierter Betreiberschritt.

**State, Zugangsdaten und Modul**

- Entfernte Pools verlangen ein S3-State-Backend; Bindung, Backend-Prüfung und Checkpoints
  bleiben. `migrate-backend` lehnt Ressourcen entfernter Pools in 8a vor jeder Änderung ab.
  Eine bestehende Ressource mit Kubernetes-State braucht vor dem Wechsel in einen entfernten
  Pool erst eine Migration nach S3.
- Geheimnisse bleiben am Rand: Der Agent hat je Team ein lokales Profil mit getrennten
  Provider- und Backend-Zugangsdaten; der Claim enthält nur nicht geheime Daten. Dafür lädt
  der Runner nicht mehr unbedingt PostgreSQL- und Kubernetes-Zugangsdaten. Zugangsdaten
  entstehen über einen Output-Vertrag des Moduls (Grundlage auch für Punkt 5).
- Der Agent holt das Modul selbst zur gepinnten Commit-Revision und einem Modulpfad
  (öffentlich oder mit lokalem Deploy-Key); Provider direkt oder über einen Mirror mit
  konfigurierbarem CA-Pfad.

**Tokens, TLS und Agent**

- Je Agent eine Kennung und ein Token, nur als Hash gespeichert, einmalig ausgegeben,
  rotierbar mit Überlappung und widerrufbar, wirksam auf allen Endpunkt-Replikas. Laufende
  Leases eines widerrufenen Tokens enden. Getrennt von den heutigen Gateway-Tokens, die im
  Klartext liegen.
- TLS mit externem Routing, Hostnamen-Prüfung, CA-Bootstrap und -Erneuerung; Weiterleitungen
  werden nicht verfolgt.
- `panko-agent` auf Basis des bestehenden Runners, Parallelität standardmäßig 1, je
  Ausführung eigenes Arbeitsverzeichnis und eigene Umgebung, Aufräumen danach. Eine
  Vorabprüfung (Endpunkt, Token, Backend, Modul, Provider) gehört zu 8a; 8b liefert sie als
  `doctor`, dazu systemd-Unit und Deployment-Manifest für einen entfernten Cluster.

Abnahme 8a: Ein Agent in Docker außerhalb des Lab-Clusters provisioniert eine
PostgreSQL-Datenbank in einem Netz, das der Cluster nicht erreicht. Anlegen, Update,
Drift-Prüfung, Wiederholen und Löschen laufen über Gateway und CLI wie heute, ohne Jobs.
Ein während eines Applies beendeter Agent führt nach Lease-Ablauf zu `interrupted`; nach dem
dokumentierten Wiederherstellungsschritt schließt ein zweiter Agent den Retry ab, das
verspätete Ergebnis des ersten wird abgewiesen. Identische Commits sind idempotent. Ein Token
eines anderen Pools beansprucht keine Runs fremder Teams, und ein Agent kann kein fremdes
Secret-Ziel setzen. Im `Run`-Objekt und im Status stehen keine Zugangsdaten. Ein
Pool-Wechsel in `panko.json` verschiebt keine bestehende Ressource.

### 8c. Nachweis gegen eine emulierte Cloud-API

Moto-Server (Apache-2.0, offizielles Image, Digest gepinnt) emuliert AWS. LocalStack
verlangt inzwischen ein Auth-Token, und Azurite deckt nur Storage ab, nicht die ARM-API
für `azurerm`.

- Testmodul im Repository (`test/modules/aws-s3`) statt eines zweiten öffentlichen Moduls,
  mit gepinntem `hashicorp/aws` und einem `aws_s3_bucket` mit Tags. Endpunkt-Override
  `endpoints.s3`, `s3_use_path_style`, übersprungene Credential-, Konto- und Regionsprüfung,
  keine Metadaten-Erkennung; Dummy-Zugangsdaten nur im lokalen Profil des Agenten.
- Testressourcentyp nur für die E2E-Tests, über den Output-Vertrag aus 8a.
- Moto und Agent in einem privaten Docker-Netz, ohne veröffentlichten Port und ohne
  k3d-Knoten. Der Test zeigt, dass Cluster-Pods Moto nicht erreichen und der Agent schon.
  `S3_IGNORE_SUBDOMAIN_BUCKETNAME=true`.
- Eigener, vorab angelegter State-Bucket mit getrenntem Backend-Endpunkt, damit
  Provisionierung und State-Sperre unabhängig prüfbar sind; die Sperre über bedingte
  Schreibvorgänge wird vorher verifiziert.

Abnahme 8c: Über Gateway und CLI einen Bucket anlegen, Tags ändern, eine Änderung am Bucket
über einen Helfer im privaten Netz als Drift erkennen, den Graph abrufen, einen gezielten
Fehler wiederholen und löschen. Geprüft wird der tatsächliche Zustand in Moto. Der
PostgreSQL-Fall bleibt der Nachweis für erzeugte Zugangsdaten. Der Nachweis zeigt die
Zusammenarbeit mit einem echten Cloud-Provider gegen eine emulierte API, nicht die
Kompatibilität mit echtem AWS.

Später: Pool-Wahl je Ressource (`spec.runnerPool`), Streaming statt Long-Poll,
`migrate-backend` über entfernte Runner.

## Entscheidungen

- Entfernte Runner arbeiten nach dem Pull-Modell; der Cluster braucht keinen Zugang zu
  entfernten Clustern oder Hosts. Jobs im Cluster bleiben der Standard.
- Egress-Regeln sind optional und standardmäßig aus; Downloads laufen über Mirrors.
- Fehlerrückmeldung vor Paketierung: Der Betreiber nutzt die bestehende Installation bereits.
- Ein zweiter Ressourcentyp erst mit einem tatsächlich benötigten Modul.
- Oberfläche erst, wenn Status und Wiederholen aussagekräftig sind; kein Graph-first.

## Bewusst nicht geplant

OIDC und Unternehmensberechtigungen, weitere Backend-Typen, von Teams mitgebrachter Code,
automatische Drift-Reparatur, ein mitgelieferter Monitoring-Stack, Ersatz von Metacontroller,
ein geteiltes Plugin-Cache-Verzeichnis für parallele Jobs.
