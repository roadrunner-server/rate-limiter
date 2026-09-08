<?php

declare(strict_types=1);

use Nyholm\Psr7\Factory\Psr17Factory;
use Nyholm\Psr7\Response;
use Spiral\RoadRunner\Http\PSR7Worker;
use Spiral\RoadRunner\Worker;

require __DIR__ . '/vendor/autoload.php';

$factory = new Psr17Factory();
$http = new PSR7Worker(Worker::create(), $factory, $factory, $factory);
$counter = 0;

while ($http->waitRequest() !== null) {
    $http->respond(new Response(200, ['Content-Type' => 'text/plain; charset=utf-8'], (string) ++$counter));
}
