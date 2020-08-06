package tests

/*
#include <openssl/ssl.h>
int X_SSL_get_security_level()
{
	int ret = -1;
	SSL_CTX *ctx = SSL_CTX_new(TLS_method());
	SSL *ssl = SSL_new(ctx);
	if(ssl == NULL)
		return ret;

	ret = SSL_get_security_level(ssl);

	if(ssl != NULL)
		SSL_free(ssl);

	if(ctx != NULL)
	    SSL_CTX_free(ctx);
	return ret;
}
#cgo LDFLAGS: -lssl
*/
import "C"

var OpenSSLSecurityLevel = C.X_SSL_get_security_level()
