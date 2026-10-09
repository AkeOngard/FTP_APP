export namespace core {
	
	export class Caps {
	    browse: boolean;
	    modify: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Caps(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.browse = source["browse"];
	        this.modify = source["modify"];
	    }
	}
	export class Site {
	    name: string;
	    protocol: string;
	    host: string;
	    port: number;
	    user: string;
	    keyFile: string;
	    skipTlsVerify: boolean;
	    blockSize: number;
	    parallel: number;
	
	    static createFrom(source: any = {}) {
	        return new Site(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.protocol = source["protocol"];
	        this.host = source["host"];
	        this.port = source["port"];
	        this.user = source["user"];
	        this.keyFile = source["keyFile"];
	        this.skipTlsVerify = source["skipTlsVerify"];
	        this.blockSize = source["blockSize"];
	        this.parallel = source["parallel"];
	    }
	}
	export class TFTPConfig {
	    autoStart: boolean;
	    bindAddr: string;
	    port: number;
	    root: string;
	    readOnly: boolean;
	
	    static createFrom(source: any = {}) {
	        return new TFTPConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.autoStart = source["autoStart"];
	        this.bindAddr = source["bindAddr"];
	        this.port = source["port"];
	        this.root = source["root"];
	        this.readOnly = source["readOnly"];
	    }
	}
	export class SFTPConfig {
	    autoStart: boolean;
	    bindAddr: string;
	    port: number;
	    root: string;
	    readOnly: boolean;
	
	    static createFrom(source: any = {}) {
	        return new SFTPConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.autoStart = source["autoStart"];
	        this.bindAddr = source["bindAddr"];
	        this.port = source["port"];
	        this.root = source["root"];
	        this.readOnly = source["readOnly"];
	    }
	}
	export class FTPConfig {
	    autoStart: boolean;
	    bindAddr: string;
	    port: number;
	    root: string;
	    readOnly: boolean;
	    passiveStart: number;
	    passiveEnd: number;
	    publicHost: string;
	    allowAnonymous: boolean;
	    tls: boolean;
	    requireTls: boolean;
	
	    static createFrom(source: any = {}) {
	        return new FTPConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.autoStart = source["autoStart"];
	        this.bindAddr = source["bindAddr"];
	        this.port = source["port"];
	        this.root = source["root"];
	        this.readOnly = source["readOnly"];
	        this.passiveStart = source["passiveStart"];
	        this.passiveEnd = source["passiveEnd"];
	        this.publicHost = source["publicHost"];
	        this.allowAnonymous = source["allowAnonymous"];
	        this.tls = source["tls"];
	        this.requireTls = source["requireTls"];
	    }
	}
	export class User {
	    name: string;
	    passwordHash: string;
	    readOnly: boolean;
	
	    static createFrom(source: any = {}) {
	        return new User(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.passwordHash = source["passwordHash"];
	        this.readOnly = source["readOnly"];
	    }
	}
	export class Config {
	    users: User[];
	    ftp: FTPConfig;
	    sftp: SFTPConfig;
	    tftp: TFTPConfig;
	    sites: Site[];
	
	    static createFrom(source: any = {}) {
	        return new Config(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.users = this.convertValues(source["users"], User);
	        this.ftp = this.convertValues(source["ftp"], FTPConfig);
	        this.sftp = this.convertValues(source["sftp"], SFTPConfig);
	        this.tftp = this.convertValues(source["tftp"], TFTPConfig);
	        this.sites = this.convertValues(source["sites"], Site);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ConnectParams {
	    protocol: string;
	    host: string;
	    port: number;
	    user: string;
	    password: string;
	    keyFile: string;
	    keyPassphrase: string;
	    skipTlsVerify: boolean;
	    timeoutSecs: number;
	    blockSize: number;
	    parallel: number;
	
	    static createFrom(source: any = {}) {
	        return new ConnectParams(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.protocol = source["protocol"];
	        this.host = source["host"];
	        this.port = source["port"];
	        this.user = source["user"];
	        this.password = source["password"];
	        this.keyFile = source["keyFile"];
	        this.keyPassphrase = source["keyPassphrase"];
	        this.skipTlsVerify = source["skipTlsVerify"];
	        this.timeoutSecs = source["timeoutSecs"];
	        this.blockSize = source["blockSize"];
	        this.parallel = source["parallel"];
	    }
	}
	
	export class FinishedTransfer {
	    service: string;
	    user: string;
	    remote: string;
	    dir: string;
	    name: string;
	    bytes: number;
	    total: number;
	    secs: number;
	    speed: number;
	    error: string;
	    // Go type: time
	    ended: any;
	
	    static createFrom(source: any = {}) {
	        return new FinishedTransfer(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.service = source["service"];
	        this.user = source["user"];
	        this.remote = source["remote"];
	        this.dir = source["dir"];
	        this.name = source["name"];
	        this.bytes = source["bytes"];
	        this.total = source["total"];
	        this.secs = source["secs"];
	        this.speed = source["speed"];
	        this.error = source["error"];
	        this.ended = this.convertValues(source["ended"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class LogEntry {
	    // Go type: time
	    time: any;
	    level: string;
	    service: string;
	    message: string;
	
	    static createFrom(source: any = {}) {
	        return new LogEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.time = this.convertValues(source["time"], null);
	        this.level = source["level"];
	        this.service = source["service"];
	        this.message = source["message"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class RemoteEntry {
	    name: string;
	    size: number;
	    isDir: boolean;
	    isLink: boolean;
	    // Go type: time
	    modTime: any;
	    mode: string;
	
	    static createFrom(source: any = {}) {
	        return new RemoteEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.size = source["size"];
	        this.isDir = source["isDir"];
	        this.isLink = source["isLink"];
	        this.modTime = this.convertValues(source["modTime"], null);
	        this.mode = source["mode"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class ServiceStatus {
	    name: string;
	    running: boolean;
	    addr: string;
	
	    static createFrom(source: any = {}) {
	        return new ServiceStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.running = source["running"];
	        this.addr = source["addr"];
	    }
	}
	
	
	export class TransferStatus {
	    id: string;
	    service: string;
	    user: string;
	    remote: string;
	    dir: string;
	    name: string;
	    bytes: number;
	    total: number;
	    speed: number;
	    etaSecs: number;
	    // Go type: time
	    started: any;
	
	    static createFrom(source: any = {}) {
	        return new TransferStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.service = source["service"];
	        this.user = source["user"];
	        this.remote = source["remote"];
	        this.dir = source["dir"];
	        this.name = source["name"];
	        this.bytes = source["bytes"];
	        this.total = source["total"];
	        this.speed = source["speed"];
	        this.etaSecs = source["etaSecs"];
	        this.started = this.convertValues(source["started"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class TransferSnapshot {
	    active: TransferStatus[];
	    recent: FinishedTransfer[];
	
	    static createFrom(source: any = {}) {
	        return new TransferSnapshot(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.active = this.convertValues(source["active"], TransferStatus);
	        this.recent = this.convertValues(source["recent"], FinishedTransfer);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	

}

export namespace main {
	
	export class HostPrompt {
	    host: string;
	    keyType: string;
	    fingerprint: string;
	
	    static createFrom(source: any = {}) {
	        return new HostPrompt(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.host = source["host"];
	        this.keyType = source["keyType"];
	        this.fingerprint = source["fingerprint"];
	    }
	}
	export class ConnInfo {
	    id: string;
	    protocol: string;
	    caps: core.Caps;
	    cwd: string;
	    unknownHost?: HostPrompt;
	
	    static createFrom(source: any = {}) {
	        return new ConnInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.protocol = source["protocol"];
	        this.caps = this.convertValues(source["caps"], core.Caps);
	        this.cwd = source["cwd"];
	        this.unknownHost = this.convertValues(source["unknownHost"], HostPrompt);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class Info {
	    version: string;
	    configDir: string;
	    os: string;
	
	    static createFrom(source: any = {}) {
	        return new Info(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.configDir = source["configDir"];
	        this.os = source["os"];
	    }
	}
	export class LocalEntry {
	    name: string;
	    size: number;
	    isDir: boolean;
	    // Go type: time
	    modTime: any;
	
	    static createFrom(source: any = {}) {
	        return new LocalEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.size = source["size"];
	        this.isDir = source["isDir"];
	        this.modTime = this.convertValues(source["modTime"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class LocalListing {
	    path: string;
	    parent: string;
	    entries: LocalEntry[];
	
	    static createFrom(source: any = {}) {
	        return new LocalListing(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.parent = source["parent"];
	        this.entries = this.convertValues(source["entries"], LocalEntry);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class RemoteItem {
	    name: string;
	    path: string;
	    isDir: boolean;
	    size: number;
	
	    static createFrom(source: any = {}) {
	        return new RemoteItem(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.path = source["path"];
	        this.isDir = source["isDir"];
	        this.size = source["size"];
	    }
	}
	export class TransferInfo {
	    id: string;
	    name: string;
	    direction: string;
	    state: string;
	    file: string;
	    done: number;
	    total: number;
	    overallDone: number;
	    overallTotal: number;
	    files: number;
	    activeFiles: number;
	    filesTotal: number;
	    speed: number;
	    etaSecs: number;
	    startedAt: number;
	    endedAt: number;
	    error: string;
	
	    static createFrom(source: any = {}) {
	        return new TransferInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.direction = source["direction"];
	        this.state = source["state"];
	        this.file = source["file"];
	        this.done = source["done"];
	        this.total = source["total"];
	        this.overallDone = source["overallDone"];
	        this.overallTotal = source["overallTotal"];
	        this.files = source["files"];
	        this.activeFiles = source["activeFiles"];
	        this.filesTotal = source["filesTotal"];
	        this.speed = source["speed"];
	        this.etaSecs = source["etaSecs"];
	        this.startedAt = source["startedAt"];
	        this.endedAt = source["endedAt"];
	        this.error = source["error"];
	    }
	}

}

