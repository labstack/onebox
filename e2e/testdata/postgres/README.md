# postgres fixture

The project the server end-to-end suite deploys.

`secrets/age.key` is a committed private key, and that is deliberate. It
decrypts `secrets/backup.env`, which holds the credentials for a MinIO the test
starts on a throwaway guest and deletes afterwards. Both are generated for this
fixture and are valid nowhere else. A real project keeps its age key out of the
repository.

`ob.yml.tmpl` is rendered at test time: the server address and the object-store
endpoint are only known once the guest is running.
