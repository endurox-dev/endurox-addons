/**
 * @brief Service "object" routines
 *
 * @file dispatch.go
 */
/* -----------------------------------------------------------------------------
 * Enduro/X Middleware Platform for Distributed Transaction Processing
 * Copyright (C) 2009-2016, ATR Baltic, Ltd. All Rights Reserved.
 * Copyright (C) 2017-2018, Mavimax, Ltd. All Rights Reserved.
 * This software is released under one of the following licenses:
 * AGPL or Mavimax's license for commercial use.
 * -----------------------------------------------------------------------------
 * AGPL license:
 *
 * This program is free software; you can redistribute it and/or modify it under
 * the terms of the GNU Affero General Public License, version 3 as published
 * by the Free Software Foundation;
 *
 * This program is distributed in the hope that it will be useful, but WITHOUT ANY
 * WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS FOR A
 * PARTICULAR PURPOSE. See the GNU Affero General Public License, version 3
 * for more details.
 *
 * You should have received a copy of the GNU Affero General Public License along
 * with this program; if not, write to the Free Software Foundation, Inc.,
 * 59 Temple Place, Suite 330, Boston, MA 02111-1307 USA
 *
 * -----------------------------------------------------------------------------
 * A commercial use license is available from Mavimax, Ltd
 * contact@mavimax.com
 * -----------------------------------------------------------------------------
 */
package main

import (
	"bytes"
	"crypto/tls"
	"io/ioutil"
	//	"io"
	"exutil"
	"net"
	"net/http"
	"strconv"
	"time"
	u "ubftab"

	atmi "github.com/endurox-dev/endurox-go"
)

//Map HTTP Error
func MapHttpError(ac *atmi.ATMICtx, svc *ServiceMap, statusCode int) int {

	var netCode int = atmi.TPEINVAL
	var lookup map[string]*int
	//Map the resposne codes
	if len(svc.Errors_fmt_http_map) > 0 {
		lookup = svc.Errors_fmt_http_map
	} else {
		lookup = Mdefaults.Errors_fmt_http_map
	}

	if nil != lookup[strconv.Itoa(statusCode)] {

		netCode = *lookup[strconv.Itoa(statusCode)]
		ac.TpLogDebug("Exact match found, converted to: %d",
			netCode)
	} else {
		//This is must have in buffer...
		netCode = *lookup["*"]

		ac.TpLogDebug("Matched wildcard \"*\", converted to: %d",
			netCode)
	}

	return netCode
}

//Dispatch service call
//@param pool     XATMI context pool
//@param nr     Number our object in pool
//@param ctxData        call context data
//@param buf            ATMI buffer with call data
//@param cd             Call descriptor
func XATMIDispatchCall(pool *XATMIPool, nr int, ctxData *atmi.TPSRVCTXDATA,
	buf *atmi.ATMIBuf, cd int, svcName string) {

	ret := SUCCEED
	ac := pool.ctxs[nr]
	buftype := ""
	subtype := ""
	var retFlags int64 = 0
	/* The error codes sent from network */
	netCode := atmi.TPMINVAL
	netMessage := ""

	//Locate our service defintion
	svc := Mservices[svcName]

	//List all buffers here..
	var bufu *atmi.TypedUBF
	var bufuRsp *atmi.TypedUBF
	var bufj *atmi.TypedJSON
	var bufs *atmi.TypedString
	var bufc *atmi.TypedCarray
	var bufv *atmi.TypedVIEW
	var bufvRsp *atmi.TypedVIEW

	retBuf := buf

	bufu_rsp_parsed := false //UBF Parsed
	bufv_rsp_parsed := false //VIEW Parsed
	var errG error

	defer func() {

		ac.TpSrvFreeCtxData(ctxData)
		ac.TpLogCloseReqFile()

		if SUCCEED == ret {
			ac.TpLogInfo("Dispatch returns SUCCEED")
			ac.TpReturn(atmi.TPSUCCESS, 0, retBuf, retFlags)
		} else {
			ac.TpLogWarn("Dispatch returns FAIL")
			ac.TpReturn(atmi.TPFAIL, 0, retBuf, retFlags)
		}

		//Put back the channel
		//!!!! MUST Be last, otherwise while tpreturn completes
		//Other thread can take this object, and that makes race condition +
		//Corrpuption !!!!
		pool.freechan <- nr
	}()

	if errA := ac.TpSrvSetCtxData(ctxData, 0); nil != errA {
		ac.TpLogError("Failed to restore context data: %s",
			errA.Error())
		ret = FAIL
		return
	}

	//Change the buffer owning context
	buf.GetBuf().TpSetCtxt(ac)

	ac.TpLogInfo("Dispatching: [%s] -> %p", svcName, svc)

	if nil == svc {
		ac.TpLogError("Invalid service name [%s] - cannot resolve",
			svcName)
		ret = FAIL
		return
	}

	ac.TpLogDebug("Reallocating the incoming buffer for storing the RSP")

	if errA := buf.TpRealloc(atmi.ATMIMsgSizeMax()); nil != errA {
		ac.TpLogError("Failed to realloc buffer to: %s",
			atmi.ATMIMsgSizeMax())
		ret = FAIL
		return
	}

	//Cast the buffer to target format
	datalen, errA := ac.TpTypes(buf, &buftype, &subtype)

	if nil != errA {
		ac.TpLogError("Invalid buffer format received: %s", errA.Error())
		ret = FAIL
		return
	}

	//Currently empty one
	var content_to_send []byte
	content_type := ""

	switch buftype {
	case "UBF", "UBF32", "FML", "FML32":

		content_type = "application/json"
		ac.TpLogInfo("UBF buffer, len %d - converting to JSON & sending req",
			datalen)

		bufu, errA = ac.CastToUBF(buf)
		if errA != nil {
			ac.TpLogError("Failed to cast to UBF: %s", errA.Error())
			ret = FAIL
			return

		}

		if bufu.BPres(u.EX_NREQLOGFILE, 0) {

			//Ignore errors..
			ac.TpLogSetReqFile(buf, "", "")
		}

		json, errA := bufu.TpUBFToJSON()

		if nil == errA {
			ac.TpLogDebug("Got json to send: [%s]", json)
			//Set content to send
			content_to_send = []byte(json)
		} else {

			ac.TpLogError("Failed to cast UBF to JSON: %s", errA.Error())
			ret = FAIL
			return
		}

		if svc.Errors_int != ERRORS_HTTP && svc.Errors_int != ERRORS_JSON2UBF {

			ac.TpLogError("Invalid configuration! Sending UBF buffer "+
				"with non 'http' or 'json2ubf' buffer handling methods. "+
				" Current method: %s", svc.Errors)

			ac.UserLog("Service [%s] configuration error! Processing "+
				"buffer UBF, but errors marked as [%s]. "+
				"Must be 'json2ubf' or 'http'. Check field 'errors' "+
				"in service config block", svc.Errors)
			ret = FAIL
			return
		}

		break
	case "VIEW", "VIEW32":
		content_type = "application/json"
		ac.TpLogInfo("VIEW buffer, len %d - converting to JSON & sending req",
			datalen)

		bufv, errA = ac.CastToVIEW(buf)
		if errA != nil {
			ac.TpLogError("Failed to cast to VIEW: %s", errA.Error())
			ret = FAIL
			return

		}
		json, errA := bufv.TpVIEWToJSON(svc.View_flags)

		if nil == errA {
			ac.TpLogDebug("Got json to send: [%s]", json)
			//Set content to send
			content_to_send = []byte(json)
		} else {

			ac.TpLogError("Failed to cast UBF to JSON: %s", errA.Error())
			ret = FAIL
			return
		}

		if svc.Errors_int != ERRORS_HTTP && svc.Errors_int != ERRORS_JSON2VIEW {

			ac.TpLogError("Invalid configuration! Sending VIEW buffer "+
				"with non 'http' or 'json2view' buffer handling methods. "+
				" Current method: %s", svc.Errors)

			ac.UserLog("Service [%s] configuration error! Processing "+
				"buffer VIEW, but errors marked as [%s]. "+
				"Must be 'json2view' or 'http'. Check field 'errors' "+
				"in service config block", svc.Errors)
			ret = FAIL
			return
		}

		break
	case "STRING":
		content_type = "text/plain"
		ac.TpLogDebug("STRING buffer, len %d", datalen)

		bufs, errA = ac.CastToString(buf)
		if errA != nil {
			ac.TpLogError("Failed to cast to STRING: %s", errA.Error())
			ret = FAIL
			return
		}

		content_to_send = []byte(bufs.GetString())

		if svc.Errors_int != ERRORS_HTTP &&
			svc.Errors_int != ERRORS_TEXT &&
			svc.Errors_int != ERRORS_JSON {
			ac.TpLogError("Invalid configuration! Sending STRING buffer "+
				"with non 'text', 'json', 'http' error handling methods. "+
				" Current method: %s", svc.Errors)

			ac.UserLog("Service [%s] configuration error! Processing "+
				"buffer STRING, but errors marked as [%s]. "+
				"Must be text', 'json', 'http'. Check field 'errors' "+
				"in service config block", svc.Errors)
			ret = FAIL
			return
		}

		break
	case "JSON":
		content_type = "application/json"
		ac.TpLogDebug("JSON buffer, len %d", datalen)

		bufj, errA = ac.CastToJSON(buf)
		if errA != nil {
			ac.TpLogError("Failed to cast to JSON: %s", errA.Error())
			ret = FAIL
			return
		}

		content_to_send = bufj.GetJSON()

		if svc.Errors_int != ERRORS_HTTP &&
			svc.Errors_int != ERRORS_TEXT &&
			svc.Errors_int != ERRORS_JSON {
			ac.TpLogError("Invalid configuration! Sending JSON buffer "+
				"with non 'text', 'json', 'http' error handling methods. "+
				" Current method: %s", svc.Errors)

			ac.UserLog("Service [%s] configuration error! Processing "+
				"buffer JSON, but errors marked as [%s]. "+
				"Must be text', 'json', 'http'. Check field 'errors' "+
				"in service config block", svc.Errors)
			ret = FAIL
			return
		}

		break
	case "CARRAY":
		content_type = "application/octet-stream"
		ac.TpLogDebug("CARRAY buffer, len %d", datalen)

		bufc, errA = ac.CastToCarray(buf)
		if errA != nil {
			ac.TpLogError("Failed to cast to CARRAY: %s", errA.Error())
			ret = FAIL
			return
		}

		content_to_send = bufc.GetBytes()

		if svc.Errors_int != ERRORS_HTTP &&
			svc.Errors_int != ERRORS_TEXT &&
			svc.Errors_int != ERRORS_JSON {
			ac.TpLogError("Invalid configuration! Sending CARRAY buffer "+
				"with non 'text', 'json', 'http' error handling methods. "+
				" Current method: %s", svc.Errors)

			ac.UserLog("Service [%s] configuration error! Processing "+
				"buffer CARRAY, but errors marked as [%s]. "+
				"Must be text', 'json', 'http'. Check field 'errors' "+
				"in service config block", svc.Errors)
			ret = FAIL
			return
		}

		break
	}

	ac.TpLogInfo("Sending POST request to: [%s] skip HTTPS cert check: %t",
		svc.Url, svc.SSLInsecure)

	ac.TpLogDump(atmi.LOG_DEBUG, "Data To send", content_to_send, len(content_to_send))
	req, errReq := http.NewRequest("POST", svc.Url, bytes.NewBuffer(content_to_send))
	if nil != errReq {
		ac.TpLogError("Failed to make request object: %s", errReq.Error())
		ret = FAIL
		return
	}

	//req.Header.Set("X-Custom-Header", "myvalue")
	req.Header.Set("Content-Type", content_type)

	tr := &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: svc.SSLInsecure},
	}
	client := &http.Client{
		Timeout:   time.Second * time.Duration(svc.Timeout),
		Transport: tr}

	//measure and log request time...
	var rspWatch exutil.StopWatch

	rspWatch.Reset()

	resp, errClt := client.Do(req)

	//Log the response
	if nil != resp {
		ac.TpLogWarn("Response Status [%s]: %s (%d ms)", svc.Url,
			resp.Status, rspWatch.GetDeltaMillis())
	}

	//Avoid file descriptor leak...
	if nil != resp {
		defer resp.Body.Close()
	}

	if errClt != nil {

		//      if nil!=resp {
		//               io.Copy(ioutil.Discard, resp.Body)
		//     }
		ac.TpLogError("Got error: %s", errClt.Error())

		if err, ok := errClt.(net.Error); ok && err.Timeout() {
			//Respond with TPSOFTTIMEOUT
			ac.TpLogError("TPSOFTTIMEOUT")
			retFlags |= atmi.TPSOFTTIMEOUT
			ret = FAIL
			return
		} else {
			//Assume other error
			ret = FAIL
			return
		}
	}

	body, errN := ioutil.ReadAll(resp.Body)
	//Allow to return connection to pool
	//io.Copy(ioutil.Discard, resp.Body)

	if nil != errN {
		ac.TpLogError("Failed to read response body - dropping the "+
			"message and responding with tout: %s",
			errN)
		retFlags |= atmi.TPSOFTTIMEOUT
		ret = FAIL
		return
	}

	//If we are nont handling in http way and http is bad
	//then return fail...
	//Check the status now
	if svc.Errors_int != ERRORS_HTTP && resp.StatusCode != http.StatusOK {

		netCode = MapHttpError(ac, svc, resp.StatusCode)
		ac.TpLogError("Expected http status [%d], but got: [%s] - fail "+
			"(apply default http error mapping: %d)",
			http.StatusOK, resp.Status, netCode)

		ret = FAIL
		//Extra flags...
		switch netCode {
		case atmi.TPETIME:
			ac.TpLogInfo("got TPETIME")
			retFlags |= atmi.TPSOFTTIMEOUT
			break
		case atmi.TPENOENT:
			ac.TpLogInfo("got TPENOENT")
			retFlags |= atmi.TPSOFTNOENT
			break
		}
		return
	}

	ac.TpLogDump(atmi.LOG_DEBUG, "Got response back", body, len(body))

	stringBody := string(body)

	ac.TpLogDebug("Got string body [%s]", stringBody)

	//Process the resposne status first
	ac.TpLogInfo("Checking status code...")
	switch svc.Errors_int {
	case ERRORS_HTTP:

		ac.TpLogInfo("Error conv mode is HTTP - looking up mapping table by %s",
			resp.Status)

		netCode = MapHttpError(ac, svc, resp.StatusCode)

		break
	case ERRORS_JSON:
		//Try to find our fields into which we are interested
		var jerr error
		netCode, netMessage, jerr = JSONErrorGet(ac, &stringBody,
			svc.Errfmt_json_code, svc.Errfmt_json_msg)

		if nil != jerr {
			ac.TpLogError("Failed to parse JSON message - dropping/ "+
				"gen timeout: %s", jerr.Error())

			retFlags |= atmi.TPSOFTTIMEOUT
			ret = FAIL
			return
		}

		//Test the error fields we got
		ac.TpLogWarn("Got response from net, code=%d, msg=[%s]",
			netCode, netMessage)

		if netMessage == "" && svc.Errfmt_json_onsucc {

			ac.TpLogError("Missing response message of [%s] in json "+
				"- Dropping/timing out", svc.Errfmt_json_msg)

			retFlags |= atmi.TPSOFTTIMEOUT
			ret = FAIL
			return
		}

		break
	case ERRORS_JSON2UBF:
		//Parse the buffer (will read all data right into buffer)
		//Allocate parse buffer - it will be new (because
		//We might not want to return data in error case...)
		//...Depending on flags
		ac.TpLogDebug("Converting to UBF: [%s]", body)

		var errA atmi.ATMIError

		bufuRsp, errA = ac.NewUBF(atmi.ATMIMsgSizeMax())

		if errA != nil {
			ac.TpLogError("Failed to alloc UBF %d:[%s] - drop/timeout",
				errA.Code(), errA.Message())

			retFlags |= atmi.TPSOFTTIMEOUT
			ret = FAIL
			return
		}

		if errA = bufuRsp.TpJSONToUBF(stringBody); errA != nil {
			ac.TpLogError("Failed to conver JSON to UBF %d:[%s]",
				errA.Code(), errA.Message())

			ac.TpLogError("Failed req: [%s] - dropping msg/tout",
				stringBody)

			retFlags |= atmi.TPSOFTTIMEOUT
			ret = FAIL
			return
		}

		bufuRsp.TpLogPrintUBF(atmi.LOG_DEBUG, "Got UBF response from net")

		bufu_rsp_parsed = true

		//JSON2UBF response fields are present always
		var errU atmi.UBFError

		netCode, errU = bufuRsp.BGetInt(u.EX_IF_ECODE, 0)

		if nil != errU {
			ac.TpLogError("Missing EX_IF_ECODE: %s - assume format "+
				"error - timeout", errU.Error())
			retFlags |= atmi.TPSOFTTIMEOUT
			ret = FAIL
			return
		}

		bufuRsp.BDel(u.EX_IF_ECODE, 0)

		netMessage, errU = bufuRsp.BGetString(u.EX_IF_EMSG, 0)

		if nil != errU {
			ac.TpLogError("Missing EX_IF_EMSG: %s - assume format "+
				"error - timeout", errU.Error())
			retFlags |= atmi.TPSOFTTIMEOUT
			ret = FAIL
			return
		}

		bufuRsp.BDel(u.EX_IF_EMSG, 0)

		break
	case ERRORS_JSON2VIEW:
		//Parse the buffer (will read all data right into buffer)
		//Allocate parse buffer - it will be new (because
		//We might not want to return data in error case...)
		//...Depending on flags
		ac.TpLogDebug("Converting to VIEW: [%s]", body)

		if "{}" == string(body) {
			ac.TpLogError("JSON Content {}: - assume format " +
				"error - timeout")
			retFlags |= atmi.TPSOFTTIMEOUT
			ret = FAIL
			return
		}

		bufvRsp, errA = ac.TpJSONToVIEW(stringBody)

		if errA != nil {
			ac.TpLogError("Failed to conver JSON to VIEW %d:[%s]",
				errA.Code(), errA.Message())

			ac.TpLogError("Failed req: [%s] - dropping msg/tout",
				stringBody)

			retFlags |= atmi.TPSOFTTIMEOUT
			ret = FAIL
			return
		}

		bufv_rsp_parsed = true

		//JSON2UBF response fields are present always
		var errU atmi.UBFError

		netCode, errU = bufvRsp.BVGetInt(svc.Errfmt_view_code, 0, 0)

		if nil != errU {
			ac.TpLogError("Missing [%s]: %s - assume format "+
				"error - timeout", svc.Errfmt_view_code, errU.Error())
			retFlags |= atmi.TPSOFTTIMEOUT
			ret = FAIL
			return
		}

		//Reset the filed in view..
		bufvRsp.BVChg(svc.Errfmt_view_code, 0, 0)

		netMessage, errU = bufvRsp.BVGetString(svc.Errfmt_view_msg, 0, 0)

		if nil != errU {
			ac.TpLogError("Missing [%s]: %s - assume format "+
				"error - timeout", svc.Errfmt_view_msg, errU.Error())
			retFlags |= atmi.TPSOFTTIMEOUT
			ret = FAIL
			return
		}

		bufvRsp.BVChg(svc.Errfmt_view_msg, 0, "")

		break
	case ERRORS_TEXT:
		//Try to scanf the string
		erroCodeMsg := svc.Errfmt_text_Regexp.FindStringSubmatch(stringBody)
		if len(erroCodeMsg) < 3 {

			ac.TpLogInfo("Error fields not found in text - assume succeed")
		} else {

			ac.TpLogInfo("Parsed response code [%s] message [%s]",
				erroCodeMsg[1], erroCodeMsg[2])

			netCode, errG = strconv.Atoi(erroCodeMsg[1])

			if nil != errG {
				//Assume that is ok? Invalid format, maybe data?
				//Well better fail with timeout...
				//The format must be exact!!

				ac.TpLogError("Invalid message code %d for text!!! "+
					"- Dropping/timeout",
					erroCodeMsg[1])

				retFlags |= atmi.TPSOFTTIMEOUT
				ret = FAIL
				return

			}
			netMessage = erroCodeMsg[2]
		}
		break
	}

	//Fix up error codes
	switch netCode {
	case atmi.TPMINVAL:
		ac.TpLogInfo("got SUCCEED")
		break
	case atmi.TPETIME:
		ac.TpLogInfo("got TPETIME")
		retFlags |= atmi.TPSOFTTIMEOUT
		ret = FAIL
		break
	case atmi.TPENOENT:
		ac.TpLogInfo("got TPENOENT")
		retFlags |= atmi.TPSOFTNOENT
		ret = FAIL
		break
	default:
		ac.TpLogInfo("defaulting to TPESVCFAIL")
		ret = FAIL
		netCode = atmi.TPESVCFAIL
		break
	}

	ac.TpLogInfo("Status after remap: code: %d message: [%s]",
		netCode, netMessage)

	//Should we parse content in case of error
	//Well we could try that if we have some data returned!
	//This should be done only in http error mapping case.
	//Parse the message (if ok to do so...)

	if !svc.ParseOnError && netCode != atmi.TPMINVAL {
		ac.TpLogWarn("Request failed and 'parseonerror' is false " +
			"- not changing buffer")
		return
	}

	if SUCCEED == ret || svc.ParseOnError {

		switch buftype {
		case "UBF", "UBF32", "FML", "FML32":

			//Parse response back from JSON
			if !bufu_rsp_parsed {
				ac.TpLogDebug("Converting to UBF: [%s]", body)

				if errA = bufu.TpJSONToUBF(stringBody); errA != nil {
					ac.TpLogError("Failed to conver rsp "+
						"buffer from JSON->UBF%d:[%s] - dropping",
						errA.Code(), errA.Message())

					ac.UserLog("Failed to conver rsp "+
						"buffer from JSON->UBF%d:[%s] - dropping",
						errA.Code(), errA.Message())

					retFlags |= atmi.TPSOFTTIMEOUT
					ret = FAIL
					return
				}

				retBuf = bufu.GetBuf()
			} else {
				//Response is parbufused and we will answer with it
				ac.TpLogDebug("Swapping UBF bufers...")

				//Original buffer will be automatically
				//is it auto
				//Also
				retBuf = bufuRsp.GetBuf()
			}
			break
		case "VIEW", "VIEW32":

			//Parse response back from JSON
			if !bufv_rsp_parsed {
				ac.TpLogDebug("Converting to VIEW: [%s]", body)

				//Keep the original buffer in reply in case if we fail.
				bufvTmp, errA := ac.TpJSONToVIEW(stringBody)

				if errA != nil {
					ac.TpLogError("Failed to conver rsp "+
						"buffer from JSON->VIEW%d:[%s] - dropping",
						errA.Code(), errA.Message())

					ac.UserLog("Failed to conver rsp "+
						"buffer from JSON->VIEW%d:[%s] - dropping",
						errA.Code(), errA.Message())

					retFlags |= atmi.TPSOFTTIMEOUT
					ret = FAIL
					return
				}

				retBuf = bufvTmp.GetBuf()
			} else {
				//Response is parbufused and we will answer with it
				ac.TpLogInfo("Swapping UBF bufers...")

				//Original buffer will be automatically
				//is it auto
				//Also
				retBuf = bufvRsp.GetBuf()
			}
			break
		case "STRING":
			//Load response into string buffer
			if errA = bufs.SetString(stringBody); errA != nil {
				ac.TpLogError("Failed to set rsp "+
					"STRING buffer %d:[%s] - dropping",
					errA.Code(), errA.Message())

				ac.UserLog("Failed to set rsp "+
					"STRING buffer %d:[%s] - dropping",
					errA.Code(), errA.Message())

				retFlags |= atmi.TPSOFTTIMEOUT
				ret = FAIL
				return
			}
			retBuf = bufs.GetBuf()
			break
		case "JSON":
			//Load response into JSON buffer
			if errA = bufj.SetJSONText(stringBody); errA != nil {
				ac.TpLogError("Failed to set JSON rsp "+
					"buffer %d:[%s]", errA.Code(),
					errA.Message())

				ac.UserLog("Failed to set JSON rsp "+
					"buffer %d:[%s]", errA.Code(),
					errA.Message())

				retFlags |= atmi.TPSOFTTIMEOUT
				ret = FAIL
				return
			}

			retBuf = bufj.GetBuf()

			break
		case "CARRAY":
			//Load response into CARRAY buffer
			if errA = bufc.SetBytes(body); errA != nil {
				ac.TpLogError("Failed to set CARRAY rsp "+
					"buffer %d:[%s]", errA.Code(),
					errA.Message())
				ac.UserLog("Failed to set CARRAY rsp "+
					"buffer %d:[%s]", errA.Code(),
					errA.Message())

				retFlags |= atmi.TPSOFTTIMEOUT
				ret = FAIL
				return
			}

			retBuf = bufc.GetBuf()

			break
		}
	}
}

/* vim: set ts=4 sw=4 et smartindent: */
