import * as React from 'react';
import useClient from './useClient';
import { setNameFilters, filterScalesByState, setPageSize, setStreamCreates } from './client';
import { ScaleRequest, ScaleRequestState } from './generated/controller_pb';

export default function useAppScale(appName: string) {
	const client = useClient();
	const [loading, setLoading] = React.useState(true);
	const [scale, setScale] = React.useState<ScaleRequest | null>(null);
	const [error, setError] = React.useState<Error | null>(null);
	React.useEffect(
		() => {
			const cancel = client.streamScales(
				(scales: ScaleRequest[], error: Error | null) => {
					if (error) {
						setError(error);
						return;
					}
					let scale;
					if (scales.length === 0) {
						scale = new ScaleRequest();
						scale.setState(ScaleRequestState.SCALE_COMPLETE);
						return;
					} else {
						scale = scales[0];
					}
					setScale(scale);
					setLoading(false);
					setError(null);
				},
				setNameFilters(appName),
				filterScalesByState(ScaleRequestState.SCALE_COMPLETE),
				setPageSize(1),
				setStreamCreates()
			);
			return cancel;
		},
		[appName, client]
	);
	return {
		loading,
		scale,
		error
	};
}
