# Project etunl.com - Reverse proxy and network tunnel


## Proxy

I want to build a reverse proxy that will have a dynamic configuration that doesnt require restart

For example:
I have a project running on a server contains of server http://localhost:3000, web application on server http://localhost:3030, and postgres DB on server localhost:5432

I want to be able to configure the proxy so that when it receives a url with subdomain, each subdomain would map to a specific application and its protocol.
For example:

- http://server.local.env -> http://localhost:3000
- http://app.local.env -> http://localhost:3030
- db.local.env -> localhost:5432

I want to be able to run it on the same machine/cluster and call it by such domain names - probably need access to hostsfile.

The challenge is that i want to be able to call it from other machine in the same network using the same domain names - is it even possible?