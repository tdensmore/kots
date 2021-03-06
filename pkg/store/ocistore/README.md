# ocistore

The OCIStore uses an OCI-compatible image registry for persistent storage.
In addition, this store stores some data in Kubernetes objects (secrets, configmaps).

## Kubernetes Objects

To enable this store to function quickly, some data is stored in the cluster. 

### Design goals

This store must use a fixed number of configmaps and secrets per application stored, and the number will not scale with the number of versions of an app, time that an application has been running, or any other metric that's not controlled by the end user.

Activity on an application must not increase the number of objects stored in the cluster.

The store must be safe to operate with multiple replicas reading AND writing to the objects. 

Sensitive data must be stored in secrets, while non sensitive data is stored in configmaps.

Use of ephemeral storage in the pod is limited and discouraged.