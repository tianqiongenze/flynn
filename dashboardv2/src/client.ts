import { grpc } from '@improbable-eng/grpc-web';

import Config from './config';
import { ControllerClient, ServiceError, Status, ResponseStream } from './generated/controller_pb_service';
import {
	StreamAppsRequest,
	StreamAppsResponse,
	UpdateAppRequest,
	App,
	StreamReleasesRequest,
	StreamReleasesResponse,
	CreateReleaseRequest,
	Release,
	ReleaseTypeMap,
	ScaleRequest,
	StreamScalesRequest,
	StreamScalesResponse,
	ScaleRequestStateMap,
	CreateScaleRequest,
	CreateDeploymentRequest,
	Deployment,
	ExpandedDeployment,
	StreamDeploymentsRequest,
	StreamDeploymentsResponse,
	DeploymentEvent,
	LabelFilter
} from './generated/controller_pb';

export interface Client {
	// read API
	streamApps: (cb: AppsCallback, ...reqModifiers: RequestModifier<StreamAppsRequest>[]) => CancelFunc;
	streamReleases: (cb: ReleasesCallback, ...reqModifiers: RequestModifier<StreamReleasesRequest>[]) => CancelFunc;
	streamScales: (cb: ScaleRequestsCallback, ...reqModifiers: RequestModifier<StreamScalesRequest>[]) => CancelFunc;
	streamDeployments: (
		cb: DeploymentsCallback,
		...reqModifiers: RequestModifier<StreamDeploymentsRequest>[]
	) => CancelFunc;

	// write API
	updateApp: (app: App, cb: AppCallback) => CancelFunc;
	createScale: (req: CreateScaleRequest, cb: CreateScaleCallback) => CancelFunc;
	createRelease: (parentName: string, release: Release, cb: ReleaseCallback) => CancelFunc;
	createDeployment: (parentName: string, scale: CreateScaleRequest | null, cb: DeploymentCallback) => CancelFunc;
}

export type ErrorWithCode = Error & ServiceError;
export type CancelFunc = () => void;
export type AppsCallback = (apps: App[], error: ErrorWithCode | null) => void;
export type AppCallback = (app: App, error: ErrorWithCode | null) => void;
export type ReleasesCallback = (releases: Release[], error: ErrorWithCode | null) => void;
export type CreateScaleCallback = (sr: ScaleRequest, error: ErrorWithCode | null) => void;
export type ReleaseCallback = (release: Release, error: ErrorWithCode | null) => void;
export type DeploymentCallback = (deployment: Deployment, error: ErrorWithCode | null) => void;
export type ScaleRequestsCallback = (scaleRequests: ScaleRequest[], error: ErrorWithCode | null) => void;
export type DeploymentsCallback = (deployments: ExpandedDeployment[], error: ErrorWithCode | null) => void;

export type RequestModifier<T> = {
	(req: T): void;
	key: string;
};

export interface PaginatableRequest {
	getPageSize(): number;
	setPageSize(value: number): void;

	getPageToken(): string;
	setPageToken(value: string): void;
}

export function setPageSize(pageSize: number): RequestModifier<PaginatableRequest> {
	return Object.assign(
		(req: PaginatableRequest) => {
			req.setPageSize(pageSize);
		},
		{ key: `pageSize--${pageSize}` }
	);
}

export interface NameFilterable {
	clearNameFiltersList(): void;
	getNameFiltersList(): Array<string>;
	setNameFiltersList(value: Array<string>): void;
	addNameFilters(value: string, index?: number): string;
}

export function setNameFilters(...filterNames: string[]): RequestModifier<NameFilterable> {
	return Object.assign(
		(req: NameFilterable) => {
			req.setNameFiltersList(filterNames);
		},
		{ key: `nameFilters--${filterNames.join('|')}` }
	);
}

export interface CreateStreamable {
	setStreamCreates(value: boolean): void;
}

export function setStreamCreates(): RequestModifier<CreateStreamable> {
	return Object.assign(
		(req: CreateStreamable) => {
			req.setStreamCreates(true);
		},
		{ key: 'streamCreates' }
	);
}

export interface UpdateStreamable {
	setStreamUpdates(value: boolean): void;
}

export function setStreamUpdates(): RequestModifier<UpdateStreamable> {
	return Object.assign(
		(req: UpdateStreamable) => {
			req.setStreamUpdates(true);
		},
		{ key: 'streamUpdates' }
	);
}

export function listDeploymentsRequestFilterType(
	...filterTypes: Array<ReleaseTypeMap[keyof ReleaseTypeMap]>
): RequestModifier<StreamDeploymentsRequest> {
	return Object.assign(
		(req: StreamDeploymentsRequest) => {
			req.setTypeFiltersList(filterTypes);
		},
		{ key: `filterTypes--${filterTypes.join('|')}` }
	);
}

export function excludeAppsWithLabels(labels: [string, string][]): RequestModifier<StreamAppsRequest> {
	return Object.assign(
		(req: StreamAppsRequest) => {
			labels.forEach(([key, val]: [string, string]) => {
				const f = new LabelFilter();
				const e = new LabelFilter.Expression();
				e.setKey(key);
				e.addValues(val);
				e.setOp(LabelFilter.Expression.Operator.OP_NOT_IN);
				f.addExpressions(e);
				req.addLabelFilters(f);
			});
		},
		{ key: `excludeAppsWithLabels--${JSON.stringify(labels)}` }
	);
}

export function filterScalesByState(
	...stateFilters: Array<ScaleRequestStateMap[keyof ScaleRequestStateMap]>
): RequestModifier<StreamScalesRequest> {
	return Object.assign(
		(req: StreamScalesRequest) => {
			req.setStateFiltersList(stateFilters);
		},
		{ key: `stateFilters--${JSON.stringify(stateFilters)}` }
	);
}

const UnknownError: ErrorWithCode = Object.assign(new Error('Unknown error'), {
	code: grpc.Code.Unknown,
	metadata: new grpc.Metadata()
});

export function isNotFoundError(error: Error): boolean {
	return (error as ErrorWithCode).code === grpc.Code.NotFound;
}

interface Cancellable {
	cancel(): void;
}

function buildCancelFunc(req: Cancellable): CancelFunc {
	let cancelled = false;
	return () => {
		if (cancelled) return;
		cancelled = true;
		req.cancel();
	};
}

function convertServiceError(error: ServiceError): ErrorWithCode {
	return Object.assign(new Error(error.message), error);
}

function buildStatusError(s: Status): ErrorWithCode {
	return Object.assign(new Error(s.details), s);
}

function buildStreamErrorHandler<T>(stream: ResponseStream<T>, cb: (error: ErrorWithCode) => void) {
	stream.on('status', (s: Status) => {
		if (s.code !== grpc.Code.OK) {
			cb(buildStatusError(s));
		}
	});
}

const __memoizedStreams = {} as { [key: string]: ResponseStream<any> };
const __memoizedStreamUsers = {} as { [key: string]: number };
const __memoizedStreamResponses = {} as { [key: string]: any };
function memoizedStream<T>(
	contextKey: string,
	streamKey: string,
	initStream: () => ResponseStream<T>
): [ResponseStream<T>, T | undefined] {
	const key = contextKey + streamKey;
	function cleanup(streamEnded = false) {
		const n = (__memoizedStreamUsers[key] = (__memoizedStreamUsers[key] || 0) - 1);
		if (n === 0 || streamEnded) {
			delete __memoizedStreams[key];
			delete __memoizedStreamUsers[key];
			delete __memoizedStreamResponses[key];
		}
		return n;
	}

	__memoizedStreamUsers[key] = (__memoizedStreamUsers[key] || 0) + 1;

	let stream = __memoizedStreams[key];
	if (stream) {
		return [stream as ResponseStream<T>, __memoizedStreamResponses[key] as T | undefined];
	}
	stream = initStream();
	stream.on('data', (data: T) => {
		__memoizedStreamResponses[key] = data;
	});
	let cancel = stream.cancel;
	stream.on('end', () => {
		cleanup(true);
		cancel = () => {};
	});
	stream.cancel = () => {
		if (cleanup() === 0) {
			cancel();
		}
	};
	__memoizedStreams[key] = stream;
	return [stream, undefined];
}

class _Client implements Client {
	private _cc: ControllerClient;
	constructor(cc: ControllerClient) {
		this._cc = cc;
	}

	public streamApps(cb: AppsCallback, ...reqModifiers: RequestModifier<StreamAppsRequest>[]): CancelFunc {
		const streamKey = reqModifiers.map((m) => m.key).join(':');
		const [stream, lastResponse] = memoizedStream('streamApps', streamKey, () => {
			const req = new StreamReleasesRequest();
			reqModifiers.forEach((m) => m(req));
			return this._cc.streamApps(req);
		});
		stream.on('data', (response: StreamAppsResponse) => {
			cb(response.getAppsList(), null);
		});
		if (lastResponse) {
			cb(lastResponse.getAppsList(), null);
		}
		buildStreamErrorHandler(stream, (error: ErrorWithCode) => {
			cb([], error);
		});
		return buildCancelFunc(stream);
	}

	public streamReleases(cb: ReleasesCallback, ...reqModifiers: RequestModifier<StreamReleasesRequest>[]): CancelFunc {
		const streamKey = reqModifiers.map((m) => m.key).join(':');
		const [stream, lastResponse] = memoizedStream('streamReleases', streamKey, () => {
			const req = new StreamReleasesRequest();
			reqModifiers.forEach((m) => m(req));
			return this._cc.streamReleases(req);
		});
		stream.on('data', (response: StreamReleasesResponse) => {
			cb(response.getReleasesList(), null);
		});
		if (lastResponse) {
			cb(lastResponse.getReleasesList(), null);
		}
		buildStreamErrorHandler(stream, (error: ErrorWithCode) => {
			cb([], error);
		});
		return buildCancelFunc(stream);
	}

	public streamScales(cb: ScaleRequestsCallback, ...reqModifiers: RequestModifier<StreamScalesRequest>[]): CancelFunc {
		const streamKey = reqModifiers.map((m) => m.key).join(':');
		const [stream, lastResponse] = memoizedStream('streamScales', streamKey, () => {
			const req = new StreamScalesRequest();
			reqModifiers.forEach((m) => m(req));
			return this._cc.streamScales(req);
		});
		stream.on('data', (response: StreamScalesResponse) => {
			cb(response.getScaleRequestsList(), null);
		});
		if (lastResponse) {
			cb(lastResponse.getScaleRequestsList(), null);
		}
		buildStreamErrorHandler(stream, (error: ErrorWithCode) => {
			cb([], error);
		});
		return buildCancelFunc(stream);
	}

	public streamDeployments(
		cb: DeploymentsCallback,
		...reqModifiers: RequestModifier<StreamDeploymentsRequest>[]
	): CancelFunc {
		const streamKey = reqModifiers.map((m) => m.key).join(':');
		const [stream, lastResponse] = memoizedStream('streamDeployments', streamKey, () => {
			const req = new StreamDeploymentsRequest();
			reqModifiers.forEach((m) => m(req));
			return this._cc.streamDeployments(req);
		});
		stream.on('data', (response: StreamDeploymentsResponse) => {
			cb(response.getDeploymentsList(), null);
		});
		if (lastResponse) {
			cb(lastResponse.getDeploymentsList(), null);
		}
		buildStreamErrorHandler(stream, (error: ErrorWithCode) => {
			cb([], error);
		});
		return buildCancelFunc(stream);
	}

	public updateApp(app: App, cb: AppCallback): CancelFunc {
		// TODO(jvatic): implement update_mask to include only changed fields
		const req = new UpdateAppRequest();
		req.setApp(app);
		return buildCancelFunc(
			this._cc.updateApp(req, (error: ServiceError | null, response: App | null) => {
				if (response && error === null) {
					cb(response, null);
				} else if (error) {
					cb(new App(), convertServiceError(error));
				} else {
					cb(new App(), UnknownError);
				}
			})
		);
	}

	public createScale(req: CreateScaleRequest, cb: CreateScaleCallback): CancelFunc {
		return buildCancelFunc(
			this._cc.createScale(req, (error: ServiceError | null, response: ScaleRequest | null) => {
				if (response && error === null) {
					cb(response, null);
				} else if (error) {
					cb(new ScaleRequest(), convertServiceError(error));
				} else {
					cb(new ScaleRequest(), UnknownError);
				}
			})
		);
	}

	public createRelease(parentName: string, release: Release, cb: ReleaseCallback): CancelFunc {
		const req = new CreateReleaseRequest();
		req.setParent(parentName);
		req.setRelease(release);
		return buildCancelFunc(
			this._cc.createRelease(req, (error: ServiceError | null, response: Release | null) => {
				if (response && error === null) {
					cb(response, null);
				} else if (error) {
					cb(new Release(), convertServiceError(error));
				} else {
					cb(new Release(), UnknownError);
				}
			})
		);
	}

	public createDeployment(parentName: string, scale: CreateScaleRequest | null, cb: DeploymentCallback): CancelFunc {
		const req = new CreateDeploymentRequest();
		req.setParent(parentName);
		if (scale) {
			req.setScaleRequest(scale);
		}

		let deployment = null as Deployment | null;
		const stream = this._cc.createDeployment(req);
		stream.on('data', (event: DeploymentEvent) => {
			const d = event.getDeployment();
			if (d) {
				deployment = d;
			}
		});
		stream.on('status', (s: Status) => {
			if (s.code === grpc.Code.OK && deployment) {
				cb(deployment, null);
			} else {
				cb(new Deployment(), buildStatusError(s));
			}
		});
		stream.on('end', () => {});
		return buildCancelFunc(stream);
	}
}

const cc = new ControllerClient(Config.CONTROLLER_HOST, {});

export default new _Client(cc);
